package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aichatdeck/internal/model"
)

// visibleScopeSQL is the single expression every scoped query uses to decide
// what an agent may see. Keeping it in one place is deliberate: isolation that
// is re-derived per query is isolation that will eventually disagree with
// itself.
//
// An agent sees a record when it is
//   - global (project_id IS NULL), or
//   - in an open project, or
//   - in a closed project where the agent is an active member, or
//   - attached to the one task a live reviewer grant scopes them to
//     (RFC-1250 §8).
//
// taskColumn names the column holding the record's task_id, or "" for tables
// that have none (reviewer access then does not apply).
func visibleScopeSQL(taskColumn string) string {
	q := `(
		project_id IS NULL
		OR project_id IN (SELECT project_id FROM projects WHERE visibility = 'open')
		OR project_id IN (
			SELECT project_id FROM project_members
			WHERE agent_id = ? AND status = 'active'
		)`
	if taskColumn != "" {
		q += `
		OR ` + taskColumn + ` IN (
			SELECT scope FROM project_members
			WHERE agent_id = ? AND status = 'reviewer'
			  AND scope IS NOT NULL
			  AND (expires_at IS NULL OR expires_at > ?)
		)`
	}
	return q + `)`
}

// visibleScopeArgs returns the bind values visibleScopeSQL expects, in order.
func visibleScopeArgs(agentID string, withTaskColumn bool) []any {
	args := []any{agentID}
	if withTaskColumn {
		args = append(args, agentID, time.Now().UTC().Format(time.RFC3339))
	}
	return args
}

type MembershipStore struct{ db *sql.DB }

func NewMembershipStore(db *sql.DB) *MembershipStore { return &MembershipStore{db: db} }

const membershipColumns = `project_id, agent_id, status, invited_by, joined_at, scope, expires_at, created_at`

func scanMembership(row interface{ Scan(...any) error }) (model.ProjectMembership, error) {
	var m model.ProjectMembership
	var invitedBy, joinedAt, scope, expiresAt sql.NullString
	var createdAt string
	if err := row.Scan(&m.ProjectID, &m.AgentID, &m.Status, &invitedBy, &joinedAt, &scope, &expiresAt, &createdAt); err != nil {
		return model.ProjectMembership{}, err
	}
	if invitedBy.Valid {
		m.InvitedBy = &invitedBy.String
	}
	if scope.Valid {
		m.Scope = &scope.String
	}
	if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
		m.CreatedAt = t
	}
	if joinedAt.Valid {
		if t, err := time.Parse(time.RFC3339, joinedAt.String); err == nil {
			m.JoinedAt = &t
		}
	}
	if expiresAt.Valid {
		if t, err := time.Parse(time.RFC3339, expiresAt.String); err == nil {
			m.ExpiresAt = &t
		}
	}
	return m, nil
}

func (s *MembershipStore) Get(ctx context.Context, projectID, agentID string) (model.ProjectMembership, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+membershipColumns+` FROM project_members WHERE project_id=? AND agent_id=?`, projectID, agentID)
	m, err := scanMembership(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ProjectMembership{}, model.NotFound("no membership record")
		}
		return model.ProjectMembership{}, err
	}
	return m, nil
}

// List returns the project's roster. Callers must gate this on membership:
// a closed project's member list is itself private (RFC-1250 §5).
func (s *MembershipStore) List(ctx context.Context, projectID string) ([]model.ProjectMembership, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+membershipColumns+` FROM project_members WHERE project_id=? ORDER BY created_at, agent_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ProjectMembership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Invite records a pending invitation from the project to an agent, or — when
// invitedBy is empty — a pending application from the agent to the project.
func (s *MembershipStore) Invite(ctx context.Context, projectID, agentID, invitedBy string, req model.InviteRequest) (model.ProjectMembership, error) {
	existing, err := s.Get(ctx, projectID, agentID)
	if err == nil {
		switch existing.Status {
		case model.MemberActive, model.MemberInvited:
			return model.ProjectMembership{}, model.Conflict("agent already has a pending or active membership").
				WithDetails(map[string]string{"status": existing.Status})
		}
		// revoked / left / an expired reviewer may be invited again at any
		// time — RFC-1250 §6 imposes no cooldown.
	} else if !model.IsCode(err, model.ErrNotFound) {
		return model.ProjectMembership{}, err
	}

	now := time.Now().UTC()
	status := model.MemberInvited
	var scope, expiresAt, joinedAt any
	if req.Reviewer {
		// A reviewer grant is read-only and time-boxed, so it takes effect
		// immediately rather than waiting to be accepted.
		status = model.MemberReviewer
		scope = req.Scope
		expiresAt = now.Add(time.Duration(req.ExpiresInSeconds) * time.Second).Format(time.RFC3339)
		joinedAt = now.Format(time.RFC3339)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO project_members (`+membershipColumns+`) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id, agent_id) DO UPDATE SET
			status=excluded.status, invited_by=excluded.invited_by, joined_at=excluded.joined_at,
			scope=excluded.scope, expires_at=excluded.expires_at, created_at=excluded.created_at`,
		projectID, agentID, status, nullIfEmpty(invitedBy), joinedAt, scope, expiresAt, now.Format(time.RFC3339),
	)
	if err != nil {
		return model.ProjectMembership{}, err
	}
	return s.Get(ctx, projectID, agentID)
}

// Decide resolves a pending invitation or application. Declining removes the
// row rather than recording a status: RFC-1250 §7 has no "declined" state, and
// a fresh invitation is allowed at any time (§6).
func (s *MembershipStore) Decide(ctx context.Context, projectID, agentID string, accept bool) (model.ProjectMembership, error) {
	existing, err := s.Get(ctx, projectID, agentID)
	if err != nil {
		return model.ProjectMembership{}, err
	}
	if existing.Status != model.MemberInvited {
		return model.ProjectMembership{}, model.Conflict("membership is not pending").
			WithDetails(map[string]string{"status": existing.Status})
	}

	if !accept {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM project_members WHERE project_id=? AND agent_id=?`, projectID, agentID); err != nil {
			return model.ProjectMembership{}, err
		}
		return model.ProjectMembership{ProjectID: projectID, AgentID: agentID}, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE project_members SET status=?, joined_at=? WHERE project_id=? AND agent_id=?`,
		model.MemberActive, time.Now().UTC().Format(time.RFC3339), projectID, agentID); err != nil {
		return model.ProjectMembership{}, err
	}
	return s.Get(ctx, projectID, agentID)
}

// SetStatus revokes a member or records a voluntary departure.
func (s *MembershipStore) SetStatus(ctx context.Context, projectID, agentID, status string) (model.ProjectMembership, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE project_members SET status=?, scope=NULL, expires_at=NULL WHERE project_id=? AND agent_id=?`,
		status, projectID, agentID)
	if err != nil {
		return model.ProjectMembership{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ProjectMembership{}, model.NotFound("no membership record")
	}
	return s.Get(ctx, projectID, agentID)
}

// CanRead reports whether agentID may see the contents of projectID.
func (s *MembershipStore) CanRead(ctx context.Context, projectID, agentID string) (bool, error) {
	var visibility string
	err := s.db.QueryRowContext(ctx, `SELECT visibility FROM projects WHERE project_id=?`, projectID).Scan(&visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return false, model.NotFound("project not found")
	}
	if err != nil {
		return false, err
	}
	if visibility == model.ProjectOpen {
		return true, nil
	}
	m, err := s.Get(ctx, projectID, agentID)
	if err != nil {
		if model.IsCode(err, model.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return m.Status == model.MemberActive, nil
}
