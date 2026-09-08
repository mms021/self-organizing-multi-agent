package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

type TaskStore struct{ db *sql.DB }

func NewTaskStore(db *sql.DB) *TaskStore { return &TaskStore{db: db} }

const taskLease = 5 * time.Minute

// RequeueExpired makes abandoned work available again. It is called on task
// reads and state changes, so recovery needs no separate scheduler process.
func (s *TaskStore) RequeueExpired(ctx context.Context) error {
	return s.requeueExpiredAt(ctx, time.Now().UTC())
}

func (s *TaskStore) requeueExpiredAt(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, owner=NULL, claimed_at=NULL, lease_expires_at=NULL, claim_id=NULL, submitted_message_id=NULL
		WHERE status=? AND (claim_id IS NULL OR (lease_expires_at IS NOT NULL AND lease_expires_at <= ?))`,
		model.TaskOpen, model.TaskClaimed, now.UTC().Format(time.RFC3339))
	return err
}

// Heartbeat extends a live claim owned by agentID. An expired claim is first
// requeued, so an old worker cannot revive work already offered to others.
func (s *TaskStore) Heartbeat(ctx context.Context, taskID, agentID, claimID string) (model.Task, error) {
	if err := s.RequeueExpired(ctx); err != nil {
		return model.Task{}, err
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET lease_expires_at=? WHERE task_id=? AND status=? AND owner=? AND claim_id=? AND lease_expires_at>?`,
		now.Add(taskLease).Format(time.RFC3339), taskID, model.TaskClaimed, agentID, claimID, now.Format(time.RFC3339))
	if err != nil {
		return model.Task{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Task{}, model.Conflict("task is not actively claimed by this agent")
	}
	return s.Get(ctx, agentID, taskID)
}

// Create inserts a task, or — if idempotencyKey is set and a task with the
// same (created_by, idempotency_key) already exists — returns that existing
// task unchanged (RFC-1700 §3).
func (s *TaskStore) Create(ctx context.Context, createdBy string, req model.CreateTaskRequest, idempotencyKey string) (model.Task, error) {
	taskID := idgen.New("task")
	now := time.Now().UTC().Format(time.RFC3339)

	var deadline any
	if req.Deadline != nil {
		deadline = req.Deadline.UTC().Format(time.RFC3339)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (task_id, schema_version, objective, context, constraints, required_capabilities, required_tools, success_criteria, status, owner, team_id, project_id, created_by, idempotency_key, created_at, deadline)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, "1.0", req.Objective, toJSON(orEmptyMap(req.Context)), toJSON(orEmptySlice(req.Constraints)),
		toJSON(orEmptySlice(req.RequiredCapabilities)), toJSON(orEmptySlice(req.RequiredTools)), toJSON(orEmptySlice(req.SuccessCriteria)),
		model.TaskOpen, nil, nil, nullableString(req.ProjectID), createdBy, nullIfEmpty(idempotencyKey), now, deadline,
	)
	if err != nil {
		if idempotencyKey != "" && isUniqueViolation(err) {
			var existingID string
			row := s.db.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE created_by=? AND idempotency_key=?`, createdBy, idempotencyKey)
			if serr := row.Scan(&existingID); serr != nil {
				return model.Task{}, serr
			}
			return s.Get(ctx, createdBy, existingID)
		}
		return model.Task{}, err
	}
	return s.Get(ctx, createdBy, taskID)
}

func scanTask(row interface{ Scan(...any) error }) (model.Task, error) {
	var t model.Task
	var contextJSON, constraintsJSON, reqCapsJSON, reqToolsJSON, successJSON string
	var owner, teamID, projectID, deadline, claimedAt, leaseExpiresAt, claimID, submittedMessageID sql.NullString
	var createdAtStr string
	err := row.Scan(&t.TaskID, &t.SchemaVersion, &t.Objective, &contextJSON, &constraintsJSON, &reqCapsJSON, &reqToolsJSON,
		&successJSON, &t.Status, &owner, &teamID, &projectID, &t.CreatedBy, &createdAtStr, &deadline, &claimedAt, &leaseExpiresAt, &claimID, &submittedMessageID)
	if err != nil {
		return model.Task{}, err
	}
	if createdAt, perr := time.Parse(time.RFC3339, createdAtStr); perr == nil {
		t.CreatedAt = createdAt
	}
	t.Context = fromJSON[map[string]any](contextJSON)
	t.ClaimID = claimID.String
	t.SubmittedMessageID = submittedMessageID.String
	t.Constraints = fromJSON[[]string](constraintsJSON)
	t.RequiredCapabilities = fromJSON[[]string](reqCapsJSON)
	t.RequiredTools = fromJSON[[]string](reqToolsJSON)
	t.SuccessCriteria = fromJSON[[]string](successJSON)
	if owner.Valid {
		t.Owner = &owner.String
	}
	if teamID.Valid {
		t.TeamID = &teamID.String
	}
	if projectID.Valid {
		t.ProjectID = &projectID.String
	}
	if deadline.Valid {
		if d, err := time.Parse(time.RFC3339, deadline.String); err == nil {
			t.Deadline = &d
		}
	}
	if claimedAt.Valid {
		if v, err := time.Parse(time.RFC3339, claimedAt.String); err == nil {
			t.ClaimedAt = &v
		}
	}
	if leaseExpiresAt.Valid {
		if v, err := time.Parse(time.RFC3339, leaseExpiresAt.String); err == nil {
			t.LeaseExpiresAt = &v
		}
	}
	return t, nil
}

const taskColumns = `task_id, schema_version, objective, context, constraints, required_capabilities, required_tools, success_criteria, status, owner, team_id, project_id, created_by, created_at, deadline, claimed_at, lease_expires_at, claim_id, submitted_message_id`

// Get returns a task if the viewer may see it. A task inside a closed project
// the viewer does not belong to reports not_found, not access_denied —
// confirming a task_id exists is itself a leak (RFC-1250 §13).
func (s *TaskStore) Get(ctx context.Context, viewer, taskID string) (model.Task, error) {
	if err := s.RequeueExpired(ctx); err != nil {
		return model.Task{}, err
	}
	args := append([]any{taskID}, visibleScopeArgs(viewer, true)...)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE task_id=? AND `+visibleScopeSQL("task_id"), args...)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Task{}, model.NotFound("task not found")
		}
		return model.Task{}, err
	}
	return t, nil
}

// List returns matching tasks in ascending (created_at, task_id) order.
// Match requirements before LIMIT so unrelated work cannot hide later matches.
func (s *TaskStore) List(ctx context.Context, viewer string, filter model.TaskFilter) ([]model.Task, string, error) {
	if err := s.RequeueExpired(ctx); err != nil {
		return nil, "", err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	where := []string{visibleScopeSQL("task_id")}
	args := visibleScopeArgs(viewer, true)
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.ProjectID != nil {
		if *filter.ProjectID == "" {
			where = append(where, "project_id IS NULL")
		} else {
			where = append(where, "project_id = ?")
			args = append(args, *filter.ProjectID)
		}
	}
	if filter.Cursor != "" {
		ts, id, ok := decodeCursor(filter.Cursor)
		if !ok {
			return nil, "", model.ValidationError("invalid cursor")
		}
		where = append(where, "(created_at > ? OR (created_at = ? AND task_id > ?))")
		args = append(args, ts, ts, id)
	}

	// Empty requirements are eligible for everyone; otherwise require any
	// overlap within each supplied filter, and AND capabilities with tools.
	for _, match := range []struct {
		column string
		values []string
	}{
		{"required_capabilities", filter.RequiredCapabilities},
		{"required_tools", filter.RequiredTools},
	} {
		if len(match.values) == 0 {
			continue
		}
		column := "tasks." + match.column // fixed column names, never user input
		where = append(where, `(json_array_length(`+column+`)=0 OR EXISTS (
			SELECT 1 FROM json_each(`+column+`) AS requirement
			JOIN json_each(?) AS available ON requirement.value=available.value))`)
		args = append(args, toJSON(match.values))
	}
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE ` + strings.Join(where, " AND ")
	q += " ORDER BY created_at, task_id LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, "", err
		}
		tasks = append(tasks, t)
		if len(tasks) == limit+1 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(tasks) > limit {
		last := tasks[limit-1]
		nextCursor = encodeCursor(last.CreatedAt.Format(time.RFC3339), last.TaskID)
		tasks = tasks[:limit]
	}
	return tasks, nextCursor, nil
}

func hasAnyCapability(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if set[w] {
			return true
		}
	}
	return false
}

// Claim performs the OPEN -> CLAIMED compare-and-swap (RFC-1000 §8's own
// example of a `conflict` error).
func (s *TaskStore) Claim(ctx context.Context, taskID, agentID string) (model.Task, error) {
	if err := s.RequeueExpired(ctx); err != nil {
		return model.Task{}, err
	}
	// Resolve through the scoped read first: a task in a closed project the
	// claimer does not belong to must look absent, not merely unclaimable.
	if _, err := s.Get(ctx, agentID, taskID); err != nil {
		return model.Task{}, err
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, owner=?, claimed_at=?, lease_expires_at=?, claim_id=?, submitted_message_id=NULL WHERE task_id=? AND status=?`,
		model.TaskClaimed, agentID, now.Format(time.RFC3339), now.Add(taskLease).Format(time.RFC3339), idgen.New("claim"), taskID, model.TaskOpen)
	if err != nil {
		return model.Task{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, gerr := s.Get(ctx, agentID, taskID)
		if gerr != nil {
			return model.Task{}, gerr
		}
		return model.Task{}, model.Conflict("task is not OPEN").WithDetails(map[string]string{"status": existing.Status})
	}
	return s.Get(ctx, agentID, taskID)
}

// Verify checks the target RESULT message and transitions task status by
// verdict, all in one transaction. No independent-reviewer pool enforcement
// (RFC-1500 is out of scope) — just the basic invariant that an agent can't
// verify its own RESULT.
func (s *TaskStore) Verify(ctx context.Context, taskID, verifierID string, req model.VerifyRequest) (model.Task, model.Verification, error) {
	if _, err := s.Get(ctx, verifierID, taskID); err != nil {
		return model.Task{}, model.Verification{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, model.Verification{}, err
	}
	defer tx.Rollback()

	var status, createdBy string
	var submittedMessageID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT status, created_by, submitted_message_id FROM tasks WHERE task_id=?`, taskID).Scan(&status, &createdBy, &submittedMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, model.Verification{}, model.NotFound("task not found")
	}
	if err != nil {
		return model.Task{}, model.Verification{}, err
	}
	if status != model.TaskSubmitted {
		return model.Task{}, model.Verification{}, model.Conflict("task is not SUBMITTED").WithDetails(map[string]string{"status": status})
	}

	var msgType, msgTaskID string
	var msgSender sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT type, task_id, sender FROM messages WHERE message_id=?`, req.TargetMessageID).
		Scan(&msgType, &msgTaskID, &msgSender)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, model.Verification{}, model.ValidationError("target_message_id does not exist")
	}
	if err != nil {
		return model.Task{}, model.Verification{}, err
	}
	if msgType != "RESULT" {
		return model.Task{}, model.Verification{}, model.ValidationError("target_message_id is not a RESULT message")
	}
	if msgTaskID != taskID {
		return model.Task{}, model.Verification{}, model.ValidationError("target_message_id does not belong to this task")
	}
	if msgSender.Valid && msgSender.String == verifierID {
		return model.Task{}, model.Verification{}, model.ValidationError("verifier must not be the RESULT's sender")
	}
	if createdBy != verifierID {
		return model.Task{}, model.Verification{}, model.AccessDenied("only the task creator may verify its result")
	}
	if submittedMessageID.String != req.TargetMessageID {
		return model.Task{}, model.Verification{}, model.Conflict("target is not the accepted result for this attempt")
	}

	verID := idgen.New("verification")
	now := time.Now().UTC()
	if err := finishVerificationJob(ctx, tx, taskID, req, now); err != nil {
		return model.Task{}, model.Verification{}, err
	}
	nowStr := now.Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO verifications (verification_id, schema_version, task_id, verifier_id, target_message_id, verdict, rationale, evidence, ts)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		verID, "1.0", taskID, verifierID, req.TargetMessageID, req.Verdict, nullIfEmpty(req.Rationale), toJSON(orEmptySlice(req.Evidence)), nowStr,
	)
	if err != nil {
		return model.Task{}, model.Verification{}, err
	}

	newStatus := status
	switch req.Verdict {
	case "verified":
		newStatus = model.TaskCompleted
	case "rejected":
		newStatus = model.TaskFailed
	}
	if newStatus != status {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status=? WHERE task_id=?`, newStatus, taskID); err != nil {
			return model.Task{}, model.Verification{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return model.Task{}, model.Verification{}, err
	}

	task, err := s.Get(ctx, verifierID, taskID)
	if err != nil {
		return model.Task{}, model.Verification{}, err
	}
	verification := model.Verification{
		VerificationID:  verID,
		SchemaVersion:   "1.0",
		TaskID:          taskID,
		VerifierID:      verifierID,
		TargetMessageID: req.TargetMessageID,
		Verdict:         req.Verdict,
		Rationale:       req.Rationale,
		Evidence:        req.Evidence,
		Timestamp:       now,
	}
	return task, verification, nil
}
