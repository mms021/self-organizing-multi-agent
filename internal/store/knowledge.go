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

type KnowledgeStore struct{ db *sql.DB }

func NewKnowledgeStore(db *sql.DB) *KnowledgeStore { return &KnowledgeStore{db: db} }

const knowledgeColumns = `knowledge_id, schema_version, category, author, content, source, confidence, status, tags, refs, project_id, task_id, supersedes, superseded_by, version, created_at`

// Create publishes a record as `proposed` (RFC-1300 §10: any agent may
// propose; only VERIFY can promote to verified/rejected). When the request
// supersedes an earlier record, both are linked and the old one is archived
// in the same transaction (RFC-1300 §9).
func (s *KnowledgeStore) Create(ctx context.Context, author string, req model.CreateKnowledgeRequest) (model.Knowledge, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Knowledge{}, err
	}
	defer tx.Rollback()

	version := 1
	if req.Supersedes != nil {
		var prevVersion int
		var prevSupersededBy sql.NullString
		var prevProject, prevTask sql.NullString
		// Check mutation rights inside the transaction. Reviewer grants are
		// read-only and must not authorize replacing an existing record.
		args := append([]any{*req.Supersedes}, visibleScopeArgs(author, false)...)
		err := tx.QueryRowContext(ctx, `SELECT version, superseded_by, project_id, task_id FROM knowledge WHERE knowledge_id=? AND `+visibleScopeSQL(""), args...).
			Scan(&prevVersion, &prevSupersededBy, &prevProject, &prevTask)
		if errors.Is(err, sql.ErrNoRows) {
			return model.Knowledge{}, model.NotFound("knowledge entry not found")
		}
		if err != nil {
			return model.Knowledge{}, err
		}
		if prevProject.Valid != (req.ProjectID != nil) || (prevProject.Valid && prevProject.String != *req.ProjectID) ||
			prevTask.Valid != (req.TaskID != nil) || (prevTask.Valid && prevTask.String != *req.TaskID) {
			return model.Knowledge{}, model.ValidationError("replacement must preserve project_id and task_id")
		}
		if prevProject.Valid {
			var status string
			if err := tx.QueryRowContext(ctx, `SELECT status FROM projects WHERE project_id=?`, prevProject.String).Scan(&status); err != nil {
				return model.Knowledge{}, err
			}
			if status != model.ProjectActive {
				return model.Knowledge{}, model.Conflict("cannot replace knowledge in an archived project")
			}
		}
		if prevSupersededBy.Valid {
			return model.Knowledge{}, model.Conflict("that record has already been superseded").
				WithDetails(map[string]string{"superseded_by": prevSupersededBy.String})
		}
		version = prevVersion + 1
	}

	if req.ProjectID != nil {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE project_id=?`, *req.ProjectID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return model.Knowledge{}, model.ValidationError("project_id does not exist")
			}
			return model.Knowledge{}, err
		}
	}

	id := idgen.New("knowledge")
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO knowledge (`+knowledgeColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "1.0", req.Category, author, toJSON(orEmptyMap(req.Content)), nullIfEmpty(req.Source),
		req.Confidence, model.KnowledgeProposed, toJSON(orEmptySlice(req.Tags)),
		toJSON(orEmptySlice(req.References)), nullableString(req.ProjectID), nullableString(req.TaskID),
		nullableString(req.Supersedes), nil, version, now.Format(time.RFC3339),
	)
	if err != nil {
		return model.Knowledge{}, err
	}

	if req.Supersedes != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE knowledge SET superseded_by=?, status=? WHERE knowledge_id=?`,
			id, model.KnowledgeArchived, *req.Supersedes); err != nil {
			return model.Knowledge{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return model.Knowledge{}, err
	}
	return s.Get(ctx, author, id)
}

func scanKnowledge(row interface{ Scan(...any) error }) (model.Knowledge, error) {
	var k model.Knowledge
	var contentJSON, tagsJSON, refsJSON, createdAtStr string
	var source, projectID, taskID, supersedes, supersededBy sql.NullString
	err := row.Scan(&k.KnowledgeID, &k.SchemaVersion, &k.Category, &k.Author, &contentJSON, &source,
		&k.Confidence, &k.Status, &tagsJSON, &refsJSON, &projectID, &taskID,
		&supersedes, &supersededBy, &k.Version, &createdAtStr)
	if err != nil {
		return model.Knowledge{}, err
	}
	k.Content = fromJSON[map[string]any](contentJSON)
	k.Tags = fromJSON[[]string](tagsJSON)
	k.References = fromJSON[[]model.Reference](refsJSON)
	if source.Valid {
		k.Source = source.String
	}
	if projectID.Valid {
		k.ProjectID = &projectID.String
	}
	if taskID.Valid {
		k.TaskID = &taskID.String
	}
	if supersedes.Valid {
		k.Supersedes = &supersedes.String
	}
	if supersededBy.Valid {
		k.SupersededBy = &supersededBy.String
	}
	if t, perr := time.Parse(time.RFC3339, createdAtStr); perr == nil {
		k.CreatedAt = t
	}
	return k, nil
}

// Get returns one record if the viewer may see it. A record inside a closed
// project the viewer does not belong to reports not_found rather than
// access_denied: confirming that a knowledge_id exists is itself a leak
// (RFC-1250 §13).
func (s *KnowledgeStore) Get(ctx context.Context, viewer, id string) (model.Knowledge, error) {
	args := append([]any{id}, visibleScopeArgs(viewer, true)...)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+knowledgeColumns+` FROM knowledge WHERE knowledge_id=? AND `+visibleScopeSQL("task_id"), args...)
	k, err := scanKnowledge(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Knowledge{}, model.NotFound("knowledge entry not found")
		}
		return model.Knowledge{}, err
	}
	return k, nil
}

// Review moves proposed -> reviewed (RFC-1300 §4). Anyone with access may
// review; promoting to verified/rejected is not possible here by design.
func (s *KnowledgeStore) Review(ctx context.Context, reviewer, id string) (model.Knowledge, error) {
	// Scope the mutation itself. Checking visibility only after an unscoped
	// UPDATE would let an outsider who guessed a knowledge_id change a closed
	// record and merely receive a misleading 404 afterwards.
	args := append([]any{model.KnowledgeReviewed, id, model.KnowledgeProposed}, visibleScopeArgs(reviewer, true)...)
	res, err := s.db.ExecContext(ctx,
		`UPDATE knowledge SET status=? WHERE knowledge_id=? AND status=? AND `+visibleScopeSQL("task_id"), args...)
	if err != nil {
		return model.Knowledge{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, gerr := s.Get(ctx, reviewer, id)
		if gerr != nil {
			return model.Knowledge{}, gerr
		}
		return model.Knowledge{}, model.Conflict("only a proposed entry can be reviewed").
			WithDetails(map[string]string{"status": existing.Status})
	}
	return s.Get(ctx, reviewer, id)
}

// Search implements RFC-1300 §8. Archived and rejected records are excluded
// unless explicitly requested.
func (s *KnowledgeStore) Search(ctx context.Context, viewer string, filter model.KnowledgeFilter) ([]model.Knowledge, string, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	// Closed-project records never reach a non-member, whatever else the
	// filter asks for (RFC-1250 §11).
	where := []string{visibleScopeSQL("task_id")}
	args := visibleScopeArgs(viewer, true)

	if filter.Category != "" {
		where = append(where, "category = ?")
		args = append(args, filter.Category)
	}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	} else if !filter.IncludeInactive {
		where = append(where, "status NOT IN (?, ?)")
		args = append(args, model.KnowledgeArchived, model.KnowledgeRejected)
	}
	if filter.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, filter.TaskID)
	}
	if filter.ProjectID != nil {
		if *filter.ProjectID == "" {
			where = append(where, "project_id IS NULL") // global only
		} else {
			// Project scope includes global knowledge: an agent working in a
			// project should still see what everyone knows.
			where = append(where, "(project_id = ? OR project_id IS NULL)")
			args = append(args, *filter.ProjectID)
		}
	}
	if filter.Query != "" {
		where = append(where, "(content LIKE ? OR tags LIKE ?)")
		like := "%" + filter.Query + "%"
		args = append(args, like, like)
	}
	if filter.Cursor != "" {
		ts, id, ok := decodeCursor(filter.Cursor)
		if !ok {
			return nil, "", model.ValidationError("invalid cursor")
		}
		where = append(where, "(created_at > ? OR (created_at = ? AND knowledge_id > ?))")
		args = append(args, ts, ts, id)
	}

	q := `SELECT ` + knowledgeColumns + ` FROM knowledge`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at, knowledge_id LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var out []model.Knowledge
	for rows.Next() {
		k, err := scanKnowledge(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(out) > limit {
		last := out[limit-1]
		nextCursor = encodeCursor(last.CreatedAt.Format(time.RFC3339), last.KnowledgeID)
		out = out[:limit]
	}

	// Tag filtering runs in Go: tags is a JSON array column, and a LIKE on it
	// would match substrings across tag boundaries.
	if len(filter.Tags) > 0 {
		filtered := out[:0]
		for _, k := range out {
			if hasAnyCapability(k.Tags, filter.Tags) {
				filtered = append(filtered, k)
			}
		}
		out = filtered
	}
	return out, nextCursor, nil
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
