package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

type CredentialStore struct{ db *sql.DB }

func NewCredentialStore(db *sql.DB) *CredentialStore { return &CredentialStore{db: db} }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Issue mints a new credential for agentID. The plaintext token is returned
// once and never persisted (RFC-1600 §7) — only its sha256 hash is stored.
func (s *CredentialStore) Issue(ctx context.Context, agentID string) (model.CredentialOut, error) {
	token := idgen.Token()
	credID := idgen.New("credential")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO credentials (credential_id, agent_id, token_hash, status, issued_at) VALUES (?,?,?,?,?)`,
		credID, agentID, hashToken(token), "active", time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return model.CredentialOut{}, err
	}
	return model.CredentialOut{CredentialID: credID, Token: token}, nil
}

// LookupAgentByToken resolves a bearer token to its owning agent and
// credential_id, if the credential is active. ok=false covers both "no such
// token" and "revoked" — RFC-1400 §7 treats an invalid/expired credential as
// "register a new agent", not as an authentication error, for
// POST /agents/register specifically.
func (s *CredentialStore) LookupAgentByToken(ctx context.Context, token string) (agentID, credentialID string, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT agent_id, credential_id FROM credentials WHERE token_hash=? AND status='active'`, hashToken(token))
	err = row.Scan(&agentID, &credentialID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return agentID, credentialID, true, nil
}
