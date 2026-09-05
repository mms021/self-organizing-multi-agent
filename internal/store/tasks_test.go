package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aichatdeck/internal/db"
	"aichatdeck/internal/model"
)

func openTestDB(t *testing.T) *AgentStoreSet {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return &AgentStoreSet{
		Agents: NewAgentStore(sqlDB),
		Tasks:  NewTaskStore(sqlDB),
		Msgs:   NewMessageStore(sqlDB),
	}
}

// AgentStoreSet bundles the stores a test needs; avoids repeating wiring.
type AgentStoreSet struct {
	Agents *AgentStore
	Tasks  *TaskStore
	Msgs   *MessageStore
}

func mustCreateAgent(t *testing.T, s *AgentStore) model.Agent {
	t.Helper()
	a, err := s.Create(context.Background(), model.RegisterRequest{})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

func apiCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*model.APIError)
	if !ok {
		t.Fatalf("expected *model.APIError, got %T: %v", err, err)
	}
	return apiErr.ErrorCode
}

func TestTaskLifecycle(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	creator := mustCreateAgent(t, stores.Agents)
	claimer := mustCreateAgent(t, stores.Agents)
	other := mustCreateAgent(t, stores.Agents)

	task, err := stores.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "do the thing"}, "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.Status != model.TaskOpen {
		t.Fatalf("expected OPEN, got %s", task.Status)
	}

	claimed, err := stores.Tasks.Claim(ctx, task.TaskID, claimer.AgentID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Status != model.TaskClaimed || claimed.Owner == nil || *claimed.Owner != claimer.AgentID {
		t.Fatalf("unexpected claimed task: %+v", claimed)
	}

	// Double-claim -> conflict.
	_, err = stores.Tasks.Claim(ctx, task.TaskID, other.AgentID)
	if code := apiCode(t, err); code != model.ErrConflict {
		t.Fatalf("expected conflict, got %s", code)
	}

	// Post a RESULT message from the claimer so we have a target_message_id.
	resultMsg := model.Envelope{
		MessageID:       "msg-result-1",
		ProtocolVersion: "1.0",
		Type:            "RESULT",
		Sender:          claimer.AgentID,
		Recipient:       creator.AgentID,
		TaskID:          task.TaskID,
		Priority:        "normal",
		Payload:         []byte(`{"summary":"done","status":"success"}`),
	}
	resultMsg.Timestamp = time.Now().UTC()
	stored, _, err := stores.Msgs.Insert(ctx, resultMsg)
	if err != nil {
		t.Fatalf("insert result message: %v", err)
	}

	// Self-verification must be rejected.
	_, _, err = stores.Tasks.Verify(ctx, task.TaskID, claimer.AgentID, model.VerifyRequest{
		TargetMessageID: stored.MessageID, Verdict: "verified",
	})
	if code := apiCode(t, err); code != model.ErrValidationError {
		t.Fatalf("expected validation_error for self-verify, got %s", code)
	}

	// Wrong-type target rejected.
	claimMsg := model.Envelope{
		MessageID: "msg-claim-1", ProtocolVersion: "1.0", Type: "CLAIM",
		Sender: claimer.AgentID, Recipient: creator.AgentID, TaskID: task.TaskID, Priority: "normal",
		Payload: []byte(`{"claim_type":"task_claim","statement":"x","confidence":0.9}`),
	}
	claimMsg.Timestamp = time.Now().UTC()
	storedClaim, _, err := stores.Msgs.Insert(ctx, claimMsg)
	if err != nil {
		t.Fatalf("insert claim message: %v", err)
	}
	_, _, err = stores.Tasks.Verify(ctx, task.TaskID, creator.AgentID, model.VerifyRequest{
		TargetMessageID: storedClaim.MessageID, Verdict: "verified",
	})
	if code := apiCode(t, err); code != model.ErrValidationError {
		t.Fatalf("expected validation_error for wrong-type target, got %s", code)
	}

	// Inconclusive leaves the task CLAIMED.
	afterTask, _, err := stores.Tasks.Verify(ctx, task.TaskID, creator.AgentID, model.VerifyRequest{
		TargetMessageID: stored.MessageID, Verdict: "inconclusive",
	})
	if err != nil {
		t.Fatalf("verify inconclusive: %v", err)
	}
	if afterTask.Status != model.TaskClaimed {
		t.Fatalf("expected still CLAIMED after inconclusive, got %s", afterTask.Status)
	}

	// Verified transitions to COMPLETED.
	afterTask, verification, err := stores.Tasks.Verify(ctx, task.TaskID, creator.AgentID, model.VerifyRequest{
		TargetMessageID: stored.MessageID, Verdict: "verified",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if afterTask.Status != model.TaskCompleted {
		t.Fatalf("expected COMPLETED, got %s", afterTask.Status)
	}
	if verification.Verdict != "verified" || verification.VerifierID != creator.AgentID {
		t.Fatalf("unexpected verification: %+v", verification)
	}
}

func TestTaskIdempotentCreate(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	creator := mustCreateAgent(t, stores.Agents)

	t1, err := stores.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "a"}, "key-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t2, err := stores.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "a different objective"}, "key-1")
	if err != nil {
		t.Fatalf("replay create: %v", err)
	}
	if t1.TaskID != t2.TaskID {
		t.Fatalf("expected same task_id for replayed idempotency key, got %s vs %s", t1.TaskID, t2.TaskID)
	}
}

func TestTaskCreateRejectsTeamProjectID(t *testing.T) {
	teamID := "team-1"
	req := model.CreateTaskRequest{Objective: "x", TeamID: &teamID}
	if err := req.Validate(); err == nil {
		t.Fatal("expected validation error for non-nil team_id")
	}
}
