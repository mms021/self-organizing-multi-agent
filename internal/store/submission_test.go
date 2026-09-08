package store

import (
	"context"
	"testing"
	"time"

	"aichatdeck/internal/model"
)

func TestSubmissionFencesAttemptsAndSurvivesLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	creator := mustCreateAgent(t, s.Agents)
	worker := mustCreateAgent(t, s.Agents)
	task, err := s.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "work"}, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Tasks.Claim(ctx, task.TaskID, worker.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tasks.requeueExpiredAt(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Tasks.Claim(ctx, task.TaskID, worker.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClaimID == "" || second.ClaimID == "" || first.ClaimID == second.ClaimID {
		t.Fatal("attempt IDs must be fresh")
	}
	if _, err := s.Tasks.Heartbeat(ctx, task.TaskID, worker.AgentID, first.ClaimID); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	result := func(id, claim string) model.Envelope {
		return model.Envelope{MessageID: id, ProtocolVersion: "1.0", Type: "RESULT", Sender: worker.AgentID, Recipient: creator.AgentID, TaskID: task.TaskID, Timestamp: time.Now().UTC(), Priority: "normal", Payload: []byte(toJSON(model.ResultPayload{ClaimID: claim, Summary: "done", Status: "success"}))}
	}
	for _, claim := range []string{"", first.ClaimID} {
		if _, _, err := s.Msgs.Insert(ctx, result("invalid", claim)); err == nil {
			t.Fatal("accepted invalid attempt")
		}
		if _, err := s.Msgs.Get(ctx, "invalid"); !model.IsCode(err, model.ErrNotFound) {
			t.Fatalf("invalid result persisted: %v", err)
		}
	}
	accepted := result("accepted", second.ClaimID)
	if _, fresh, err := s.Msgs.Insert(ctx, accepted); err != nil || !fresh {
		t.Fatalf("submit: %v %v", fresh, err)
	}
	if err := s.Tasks.requeueExpiredAt(ctx, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	submitted, err := s.Tasks.Get(ctx, creator.AgentID, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Status != model.TaskSubmitted || submitted.LeaseExpiresAt != nil || submitted.SubmittedMessageID != accepted.MessageID {
		t.Fatalf("submission lost: %+v", submitted)
	}
	if _, err := s.Tasks.Claim(ctx, task.TaskID, worker.AgentID); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("submitted task reclaimed: %v", err)
	}
	if _, _, err := s.Msgs.Insert(ctx, result("second-result", second.ClaimID)); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("second result: %v", err)
	}
	if _, err := s.Msgs.Get(ctx, "second-result"); !model.IsCode(err, model.ErrNotFound) {
		t.Fatalf("second result persisted: %v", err)
	}
	if _, fresh, err := s.Msgs.Insert(ctx, accepted); err != nil || fresh {
		t.Fatalf("replay: %v %v", fresh, err)
	}
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, creator.AgentID, model.VerifyRequest{TargetMessageID: accepted.MessageID, Verdict: "verified"}); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.Msgs.Insert(ctx, accepted); err != nil || fresh {
		t.Fatalf("completed replay: %v %v", fresh, err)
	}
}

func TestExpiredResultRollsBackWithoutRequeue(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	creator := mustCreateAgent(t, s.Agents)
	worker := mustCreateAgent(t, s.Agents)
	task, err := s.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "expired"}, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Tasks.Claim(ctx, task.TaskID, worker.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.db.ExecContext(ctx, `UPDATE tasks SET lease_expires_at=? WHERE task_id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), task.TaskID); err != nil {
		t.Fatal(err)
	}
	env := model.Envelope{MessageID: "late", Type: "RESULT", Sender: worker.AgentID, Recipient: creator.AgentID, TaskID: task.TaskID, Payload: []byte(toJSON(model.ResultPayload{ClaimID: claim.ClaimID, Summary: "late", Status: "success"}))}
	if _, _, err := s.Msgs.Insert(ctx, env); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("expired result: %v", err)
	}
	if _, err := s.Msgs.Get(ctx, "late"); !model.IsCode(err, model.ErrNotFound) {
		t.Fatalf("late result persisted: %v", err)
	}
}

func TestLegacyClaimIsRequeued(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	creator := mustCreateAgent(t, s.Agents)
	task, err := s.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "legacy"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.db.ExecContext(ctx, `UPDATE tasks SET status='CLAIMED', owner=?, claim_id=NULL WHERE task_id=?`, creator.AgentID, task.TaskID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Tasks.Get(ctx, creator.AgentID, task.TaskID)
	if err != nil || got.Status != model.TaskOpen || got.Owner != nil {
		t.Fatalf("legacy recovery: %+v %v", got, err)
	}
}
