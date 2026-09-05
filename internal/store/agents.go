package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

type AgentStore struct{ db *sql.DB }

func NewAgentStore(db *sql.DB) *AgentStore { return &AgentStore{db: db} }

// buildMetadata folds the RFC-1000 §5 registration fields that have no
// dedicated top-level slot in the RFC-1800 §3 Agent shape into metadata
// (RFC-1800 §1: extension via metadata/extensions).
func buildMetadata(req model.RegisterRequest) map[string]any {
	md := map[string]any{}
	if len(req.Skills) > 0 {
		md["skills"] = req.Skills
	}
	if len(req.PreferredRoles) > 0 {
		md["preferred_roles"] = req.PreferredRoles
	}
	if len(req.Evidence) > 0 {
		md["evidence"] = req.Evidence
	}
	if req.Operator != "" {
		md["operator"] = req.Operator
	}
	return md
}

// Create inserts a brand-new agent (RFC-1400 §7: no valid credential ⇒ new
// agent). Bootstrap steps with no server-side counterpart this milestone
// (policy/schema check, shared-memory check) are skipped, so registration
// moves straight to READY.
func (s *AgentStore) Create(ctx context.Context, req model.RegisterRequest) (model.Agent, error) {
	agentID := idgen.New("agent")
	now := time.Now().UTC().Format(time.RFC3339)
	load := 0.0
	if req.CurrentLoad != nil {
		load = *req.CurrentLoad
	}
	caps := orEmptySlice(req.Capabilities)
	constraints := orEmptyMap(req.Constraints)
	metadata := buildMetadata(req)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (agent_id, schema_version, capabilities, skills, constraints, preferred_roles, evidence, operator, load, status, metadata, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		agentID, "1.0", toJSON(caps), toJSON(orEmptySlice(req.Skills)), toJSON(constraints),
		toJSON(orEmptySlice(req.PreferredRoles)), toJSON(orEmptySlice(req.Evidence)), nullIfEmpty(req.Operator),
		load, model.AgentReady, toJSON(metadata), now, now,
	)
	if err != nil {
		return model.Agent{}, err
	}
	return s.Get(ctx, agentID)
}

// Update applies newly-submitted profile fields to an already-registered
// agent (RFC-1400 §7: re-registration with a valid credential MUST apply the
// new data, not just return the cached original — RFC-1000 §5).
func (s *AgentStore) Update(ctx context.Context, agentID string, req model.RegisterRequest) (model.Agent, error) {
	load := 0.0
	if req.CurrentLoad != nil {
		load = *req.CurrentLoad
	}
	caps := orEmptySlice(req.Capabilities)
	constraints := orEmptyMap(req.Constraints)
	metadata := buildMetadata(req)
	now := time.Now().UTC().Format(time.RFC3339)

	res, err := s.db.ExecContext(ctx, `
		UPDATE agents SET capabilities=?, skills=?, constraints=?, preferred_roles=?, evidence=?, operator=?, load=?, metadata=?, updated_at=?
		WHERE agent_id=?`,
		toJSON(caps), toJSON(orEmptySlice(req.Skills)), toJSON(constraints), toJSON(orEmptySlice(req.PreferredRoles)),
		toJSON(orEmptySlice(req.Evidence)), nullIfEmpty(req.Operator), load, toJSON(metadata), now, agentID,
	)
	if err != nil {
		return model.Agent{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Agent{}, model.NotFound("agent not found")
	}
	return s.Get(ctx, agentID)
}

func (s *AgentStore) Get(ctx context.Context, agentID string) (model.Agent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT agent_id, schema_version, capabilities, constraints, load, status, metadata
		FROM agents WHERE agent_id=?`, agentID)

	var a model.Agent
	var capsJSON, constraintsJSON, metadataJSON string
	if err := row.Scan(&a.AgentID, &a.SchemaVersion, &capsJSON, &constraintsJSON, &a.Load, &a.Status, &metadataJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Agent{}, model.NotFound("agent not found")
		}
		return model.Agent{}, err
	}
	a.Capabilities = fromJSON[[]model.Capability](capsJSON)
	a.Constraints = fromJSON[map[string]any](constraintsJSON)
	a.Metadata = fromJSON[map[string]any](metadataJSON)
	a.Roles = []string{}            // Roles (RFC-1200) out of scope this milestone.
	a.Reputation = map[string]any{} // Reputation (RFC-1500) out of scope this milestone.
	return a, nil
}
