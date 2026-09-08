package store

import (
	"context"
	"sync"
	"testing"

	"aichatdeck/internal/db"
	"aichatdeck/internal/model"
)

func TestConcurrentRotationHasExactlyOneWinner(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	agent := mustCreateAgent(t, s.Agents)
	c := NewCredentialStore(s.Tasks.db)
	key, err := c.Issue(ctx, agent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		key model.CredentialOut
		err error
	}
	start := make(chan struct{})
	results := make(chan outcome, 8)
	for i := 0; i < cap(results); i++ {
		go func() { <-start; next, err := c.Rotate(ctx, key.Token); results <- outcome{next, err} }()
	}
	close(start)
	winners := 0
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err != nil {
			if !model.IsCode(result.err, model.ErrConflict) {
				t.Errorf("unexpected loser error: %v", result.err)
			}
			continue
		}
		winners++
		id, _, active, err := c.LookupAgentByToken(ctx, result.key.Token)
		if err != nil || !active || id != agent.AgentID {
			t.Errorf("winner key invalid: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d, want 1", winners)
	}
	var active, events int
	if err := s.Tasks.db.QueryRow(`SELECT count(*) FROM credentials WHERE status='active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := s.Tasks.db.QueryRow(`SELECT count(*) FROM credential_audit`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if active != 1 || events != 1 {
		t.Fatalf("active=%d events=%d", active, events)
	}
}

func TestRotationCannotEscapeConcurrentOperatorRevoke(t *testing.T) {
	for iteration := 0; iteration < 8; iteration++ {
		s := openTestDB(t)
		ctx := context.Background()
		agent := mustCreateAgent(t, s.Agents)
		c := NewCredentialStore(s.Tasks.db)
		key, err := c.Issue(ctx, agent.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		var rotateErr, revokeErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, rotateErr = c.Rotate(ctx, key.Token) }()
		go func() { defer wg.Done(); <-start; _, revokeErr = c.RevokeAgent(ctx, int64(iteration), agent.AgentID) }()
		close(start)
		wg.Wait()
		if revokeErr != nil || (rotateErr != nil && !model.IsCode(rotateErr, model.ErrConflict)) {
			t.Fatalf("rotate=%v revoke=%v", rotateErr, revokeErr)
		}
		var active int
		if err := s.Tasks.db.QueryRow(`SELECT count(*) FROM credentials WHERE agent_id=? AND status='active'`, agent.AgentID).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 0 {
			t.Fatalf("iteration %d: %d credentials escaped revoke", iteration, active)
		}
	}
}

func TestRevocationRollsBackWhenAuditFails(t *testing.T) {
	for _, operator := range []bool{false, true} {
		s := openTestDB(t)
		ctx := context.Background()
		agent := mustCreateAgent(t, s.Agents)
		c := NewCredentialStore(s.Tasks.db)
		key, err := c.Issue(ctx, agent.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tasks.db.Exec(`CREATE TRIGGER fail_revoke_audit BEFORE INSERT ON credential_audit BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
			t.Fatal(err)
		}
		if operator {
			_, err = c.RevokeAgent(ctx, 123, agent.AgentID)
		} else {
			err = c.RevokeCurrent(ctx, key.Token)
		}
		if err == nil {
			t.Fatal("revocation committed without audit")
		}
		if _, _, active, err := c.LookupAgentByToken(ctx, key.Token); err != nil || !active {
			t.Fatalf("rollback lost original key: %v", err)
		}
		var updates int
		if err := s.Tasks.db.QueryRow(`SELECT count(*) FROM credential_operator_updates`).Scan(&updates); err != nil || updates != 0 {
			t.Fatalf("failed command consumed update: %d %v", updates, err)
		}
		if _, err := s.Tasks.db.Exec(`DROP TRIGGER fail_revoke_audit`); err != nil {
			t.Fatal(err)
		}
		if operator {
			_, err = c.RevokeAgent(ctx, 123, agent.AgentID)
		} else {
			err = c.RevokeCurrent(ctx, key.Token)
		}
		if err != nil {
			t.Fatalf("retry after failure: %v", err)
		}
	}
}

func TestCredentialsAndOperatorDedupSurviveDatabaseRestart(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	agent := mustCreateAgent(t, s.Agents)
	c := NewCredentialStore(s.Tasks.db)
	old, err := c.Issue(ctx, agent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := c.Rotate(ctx, old.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RevokeAgent(ctx, 789, agent.AgentID); err != nil {
		t.Fatal(err)
	}
	recovery, err := c.Issue(ctx, agent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	if err := s.Tasks.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := s.Tasks.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	c = NewCredentialStore(reopened)
	for _, token := range []string{old.Token, rotated.Token} {
		if _, _, active, err := c.LookupAgentByToken(ctx, token); err != nil || active {
			t.Fatalf("restart resurrected a revoked credential: %v", err)
		}
	}
	if fresh, err := c.RevokeAgent(ctx, 789, agent.AgentID); err != nil || fresh {
		t.Fatalf("restart lost update ledger: %v %v", fresh, err)
	}
	if id, _, active, err := c.LookupAgentByToken(ctx, recovery.Token); err != nil || !active || id != agent.AgentID {
		t.Fatalf("replay revoked recovery credential: %v", err)
	}
	if _, err := reopened.Exec(`UPDATE credential_audit SET actor='tampered'`); err == nil {
		t.Fatal("audit update allowed after restart")
	}
	var events int
	if err := reopened.QueryRow(`SELECT count(*) FROM credential_audit`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("audit history changed: %d %v", events, err)
	}
}
