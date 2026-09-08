package store

import (
	"context"
	"testing"
)

func TestCredentialRotationRevocationAudit(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	a := mustCreateAgent(t, s.Agents)
	other := mustCreateAgent(t, s.Agents)
	c := NewCredentialStore(s.Tasks.db)
	key, err := c.Issue(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := c.Issue(ctx, other.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	// Failure writing audit must roll back both old-key revocation and issuance.
	if _, err := s.Tasks.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON credential_audit BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rotate(ctx, key.Token); err == nil {
		t.Fatal("rotation without audit")
	}
	if _, _, ok, err := c.LookupAgentByToken(ctx, key.Token); err != nil || !ok {
		t.Fatal("failed rotation revoked original key")
	}
	if _, err := s.Tasks.db.Exec(`DROP TRIGGER fail_audit`); err != nil {
		t.Fatal(err)
	}
	next, err := c.Rotate(ctx, key.Token)
	if err != nil {
		t.Fatal(err)
	}
	if next.Token == key.Token {
		t.Fatal("token unchanged")
	}
	if _, _, ok, _ := c.LookupAgentByToken(ctx, key.Token); ok {
		t.Fatal("old token still active")
	}
	if id, _, ok, err := c.LookupAgentByToken(ctx, next.Token); err != nil || !ok || id != a.AgentID {
		t.Fatalf("identity changed: %s %v", id, err)
	}
	if _, err := c.Rotate(ctx, key.Token); err == nil {
		t.Fatal("stale rotation succeeded")
	}
	if fresh, err := c.RevokeAgent(ctx, 123, a.AgentID); err != nil || !fresh {
		t.Fatalf("revoke all: %v %v", fresh, err)
	}
	if _, _, ok, _ := c.LookupAgentByToken(ctx, next.Token); ok {
		t.Fatal("revoked token active")
	}
	if _, _, ok, _ := c.LookupAgentByToken(ctx, otherKey.Token); !ok {
		t.Fatal("other agent affected")
	}
	recovery, err := c.Issue(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh, err := c.RevokeAgent(ctx, 123, a.AgentID); err != nil || fresh {
		t.Fatalf("replayed revoke: %v %v", fresh, err)
	}
	if _, _, ok, _ := c.LookupAgentByToken(ctx, recovery.Token); !ok {
		t.Fatal("replay revoked later credential")
	}
	if err := c.RevokeCurrent(ctx, recovery.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := c.LookupAgentByToken(ctx, recovery.Token); ok {
		t.Fatal("self-revoked token active")
	}
	if _, err := s.Tasks.db.Exec(`DELETE FROM credential_audit`); err == nil {
		t.Fatal("audit deletable")
	}
	var count int
	if err := s.Tasks.db.QueryRow(`SELECT count(*) FROM credential_audit`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("audit count %d %v", count, err)
	}
}
