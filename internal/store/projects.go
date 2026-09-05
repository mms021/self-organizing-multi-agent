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

const projectColumns = `project_id, schema_version, name, objective, visibility, listed, owner, status, created_at, metadata`

func (s *ProjectStore) Create(ctx context.Context, owner string, req model.CreateProjectRequest) (model.Project, error) {
	id := idgen.New("project")
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (`+projectColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, "1.0", req.Name, req.Objective, model.ProjectOpen, 1, owner, model.ProjectActive, now, "{}",
	)
	if err != nil {
		return model.Project{}, err
	}
	return s.Get(ctx, id)
}

func scanProject(row interface{ Scan(...any) error }) (model.Project, error) {
	var p model.Project
	var listed int
	var metadataJSON, createdAtStr string
	err := row.Scan(&p.ProjectID, &p.SchemaVersion, &p.Name, &p.Objective, &p.Visibility,
		&listed, &p.Owner, &p.Status, &createdAtStr, &metadataJSON)
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

// List returns every project. All projects are `open` this milestone, so
// there is nothing to hide yet — the visibility filtering of RFC-1250 §5
// arrives with closed projects.
func (s *ProjectStore) List(ctx context.Context) ([]model.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY created_at, project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
