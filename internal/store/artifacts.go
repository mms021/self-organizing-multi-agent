package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

type ArtifactStore struct{ db *sql.DB }

func NewArtifactStore(db *sql.DB) *ArtifactStore { return &ArtifactStore{db: db} }

const artifactColumns = `artifact_id, schema_version, type, uri, checksum, created_by, task_id, project_id, size, retention, expires_at, created_at`

// Create registers an artifact. There is no update path by design: an
// artifact used as evidence MUST be immutable, and a revision is a new
// artifact_id (RFC-1300 §7).
func (s *ArtifactStore) Create(ctx context.Context, createdBy string, req model.CreateArtifactRequest) (model.Artifact, error) {
	retention := req.Retention
	if retention == "" {
		retention = "permanent"
	}
	id := idgen.New("artifact")
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artifacts (`+artifactColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "1.0", req.Type, req.URI, req.Checksum, createdBy,
		nullableString(req.TaskID), nullableString(req.ProjectID), req.Size, retention, nil, now,
	)
	if err != nil {
		return model.Artifact{}, err
	}
	return s.Get(ctx, id)
}

func (s *ArtifactStore) Get(ctx context.Context, id string) (model.Artifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE artifact_id=?`, id)

	var a model.Artifact
	var taskID, projectID, expiresAt sql.NullString
	var createdAtStr string
	err := row.Scan(&a.ArtifactID, &a.SchemaVersion, &a.Type, &a.URI, &a.Checksum, &a.CreatedBy,
		&taskID, &projectID, &a.Size, &a.Retention, &expiresAt, &createdAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Artifact{}, model.NotFound("artifact not found")
		}
		return model.Artifact{}, err
	}
	if taskID.Valid {
		a.TaskID = &taskID.String
	}
	if projectID.Valid {
		a.ProjectID = &projectID.String
	}
	if expiresAt.Valid {
		if t, perr := time.Parse(time.RFC3339, expiresAt.String); perr == nil {
			a.ExpiresAt = &t
		}
	}
	if t, perr := time.Parse(time.RFC3339, createdAtStr); perr == nil {
		a.CreatedAt = t
	}
	return a, nil
}
