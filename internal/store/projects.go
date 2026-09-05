package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

type ProjectStore struct{ db *sql.DB }

func NewProjectStore(db *sql.DB) *ProjectStore { return &ProjectStore{db: db} }

const projectColumns = `project_id, schema_version, name, objective, visibility, listed, membership_policy, owner, status, created_at, metadata`

func (s *ProjectStore) Create(ctx context.Context, owner string, req model.CreateProjectRequest) (model.Project, error) {
	id := idgen.New("project")
	now := time.Now().UTC().Format(time.RFC3339)

	visibility := req.Visibility
	if visibility == "" {
		visibility = model.ProjectOpen
	}
	// An open project is visible to everyone, so `listed` is meaningless there
	// and always true; for a closed one it defaults to hidden.
	listed := visibility == model.ProjectOpen
	if req.Listed != nil {
		listed = *req.Listed
	}
	policy := req.MembershipPolicy
	if policy == "" {
		policy = model.PolicyInviteOnly
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Project{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (`+projectColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, "1.0", req.Name, req.Objective, visibility, boolToInt(listed), policy, owner, model.ProjectActive, now, "{}",
	); err != nil {
		return model.Project{}, err
	}

	// The owner is a member, recorded as one. Treating ownership as a
	// separate implicit grant would mean every visibility rule had to
	// special-case it — and the one that forgot would either hide the
	// project from its own owner or expose it to everyone.
	if visibility == model.ProjectClosed {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_members (project_id, agent_id, status, invited_by, joined_at, scope, expires_at, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			id, owner, model.MemberActive, nil, now, nil, nil, now,
		); err != nil {
			return model.Project{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return model.Project{}, err
	}
	return s.Get(ctx, id)
}

func scanProject(row interface{ Scan(...any) error }) (model.Project, error) {
	var p model.Project
	var listed int
	var metadataJSON, createdAtStr string
	err := row.Scan(&p.ProjectID, &p.SchemaVersion, &p.Name, &p.Objective, &p.Visibility,
		&listed, &p.MembershipPolicy, &p.Owner, &p.Status, &createdAtStr, &metadataJSON)
	if err != nil {
		return model.Project{}, err
	}
	p.Listed = listed != 0
	p.Metadata = fromJSON[map[string]any](metadataJSON)
	if t, perr := time.Parse(time.RFC3339, createdAtStr); perr == nil {
		p.CreatedAt = t
	}
	return p, nil
}

func (s *ProjectStore) Get(ctx context.Context, id string) (model.Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE project_id=?`, id)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Project{}, model.NotFound("project not found")
		}
		return model.Project{}, err
	}
	return p, nil
}

// List returns what agentID is allowed to discover: every open project, every
// closed project it belongs to in full, and listed closed projects reduced to
// the fact that they exist (RFC-1250 §5). Unlisted closed projects it does not
// belong to are absent entirely — discovery must not double as an enumeration
// oracle (§13).
func (s *ProjectStore) List(ctx context.Context, agentID string) ([]model.Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+projectColumns+`, (
			SELECT status FROM project_members m
			WHERE m.project_id = projects.project_id AND m.agent_id = ?
		) AS my_status
		FROM projects
		WHERE visibility = 'open' OR listed = 1 OR my_status IS NOT NULL
		ORDER BY created_at, project_id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Project
	for rows.Next() {
		var p model.Project
		var listed int
		var metadataJSON, createdAtStr string
		var myStatus sql.NullString
		if err := rows.Scan(&p.ProjectID, &p.SchemaVersion, &p.Name, &p.Objective, &p.Visibility,
			&listed, &p.MembershipPolicy, &p.Owner, &p.Status, &createdAtStr, &metadataJSON, &myStatus); err != nil {
			return nil, err
		}
		p.Listed = listed != 0
		p.Metadata = fromJSON[map[string]any](metadataJSON)
		if t, perr := time.Parse(time.RFC3339, createdAtStr); perr == nil {
			p.CreatedAt = t
		}

		member := myStatus.Valid && myStatus.String == model.MemberActive
		if p.Visibility == model.ProjectClosed && !member && p.Owner != agentID {
			p = p.Listing() // exists, nothing more
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
