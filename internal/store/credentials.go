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

// Rotate atomically invalidates the presented key and creates its replacement.
// Recheck under the transaction: middleware authentication may predate a revoke.
func (s *CredentialStore) Rotate(ctx context.Context, token string) (model.CredentialOut, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.CredentialOut{}, err
	}
	defer tx.Rollback()
	var agentID, oldID string
	if err := tx.QueryRowContext(ctx, `SELECT agent_id,credential_id FROM credentials WHERE token_hash=? AND status='active'`, hashToken(token)).Scan(&agentID, &oldID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return model.CredentialOut{}, err
		}
		return model.CredentialOut{}, model.Conflict("credential is no longer active")
	}
	out := model.CredentialOut{CredentialID: idgen.New("credential"), Token: idgen.Token()}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx, `UPDATE credentials SET status='revoked' WHERE credential_id=?`, oldID); err != nil {
		return model.CredentialOut{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO credentials(credential_id,agent_id,token_hash,status,rotated_from,issued_at) VALUES(?,?,?,'active',?,?)`, out.CredentialID, agentID, hashToken(out.Token), oldID, now); err != nil {
		return model.CredentialOut{}, err
	}
	if err = credentialAudit(ctx, tx, agentID, agentID, oldID, "rotated"); err != nil {
		return model.CredentialOut{}, err
	}
	return out, tx.Commit()
}

func credentialAudit(ctx context.Context, tx *sql.Tx, actor, agentID, credentialID, action string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO credential_audit(event_id,actor,agent_id,credential_id,action,created_at) VALUES(?,?,?,?,?,?)`, idgen.New("credential-event"), actor, agentID, credentialID, action, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *CredentialStore) RevokeCurrent(ctx context.Context, token string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var agentID, credID string
	if err := tx.QueryRowContext(ctx, `SELECT agent_id,credential_id FROM credentials WHERE token_hash=? AND status='active'`, hashToken(token)).Scan(&agentID, &credID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return model.Conflict("credential is no longer active")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE credentials SET status='revoked' WHERE credential_id=?`, credID); err != nil {
		return err
	}
	if err = credentialAudit(ctx, tx, agentID, agentID, credID, "revoked"); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeAgent is reserved for the configured operator, never exposed to agents.
func (s *CredentialStore) RevokeAgent(ctx context.Context, updateID int64, agentID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT agent_id FROM credential_operator_updates WHERE update_id=?`, updateID).Scan(&prior)
	if err == nil {
		if prior != agentID {
			return false, model.Conflict("update already used")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT agent_id FROM agents WHERE agent_id=?`, agentID).Scan(&prior); errors.Is(err, sql.ErrNoRows) {
		return false, model.NotFound("agent not found")
	} else if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE credentials SET status='revoked' WHERE agent_id=? AND status='active'`, agentID); err != nil {
		return false, err
	}
	if err = credentialAudit(ctx, tx, "telegram-operator", agentID, "*", "revoked_all"); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO credential_operator_updates(update_id,agent_id,created_at) VALUES(?,?,?)`, updateID, agentID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
