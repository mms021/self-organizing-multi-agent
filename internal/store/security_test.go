package store

import (
	"aichatdeck/internal/model"
	"context"
	"testing"
)

func TestSupersedesCannotArchivePrivateKnowledge(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	owner := mustCreateAgent(t, s.Agents)
	outsider := mustCreateAgent(t, s.Agents)
	p, err := NewProjectStore(s.Agents.db).Create(ctx, owner.AgentID, model.CreateProjectRequest{Name: "private", Visibility: model.ProjectClosed})
	if err != nil {
		t.Fatal(err)
	}
	k := NewKnowledgeStore(s.Agents.db)
	original, err := k.Create(ctx, owner.AgentID, model.CreateKnowledgeRequest{Category: model.KnowledgeLesson, Content: map[string]any{"summary": "secret"}, ProjectID: &p.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	replacement := model.CreateKnowledgeRequest{Category: model.KnowledgeLesson, Content: map[string]any{"summary": "replacement"}, Supersedes: &original.KnowledgeID}
	if _, err := k.Create(ctx, outsider.AgentID, replacement); !model.IsCode(err, model.ErrNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := k.Create(ctx, owner.AgentID, replacement); !model.IsCode(err, model.ErrValidationError) {
		t.Fatalf("scope change: %v", err)
	}
	unchanged, err := k.Get(ctx, owner.AgentID, original.KnowledgeID)
	if err != nil || unchanged.Status != model.KnowledgeProposed || unchanged.SupersededBy != nil {
		t.Fatalf("original mutated: %+v %v", unchanged, err)
	}
	replacement.ProjectID = &p.ProjectID
	if _, err := k.Create(ctx, owner.AgentID, replacement); err != nil {
		t.Fatal(err)
	}
}

func TestForeignMessageReplayDoesNotExposePayload(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	a := mustCreateAgent(t, s.Agents)
	b := mustCreateAgent(t, s.Agents)
	env := model.Envelope{MessageID: "private-id", Sender: a.AgentID, Recipient: a.AgentID, Type: "EVENT", Payload: []byte(`{"event_type":"secret"}`)}
	if _, _, err := s.Msgs.Insert(ctx, env); err != nil {
		t.Fatal(err)
	}
	env.Sender = b.AgentID
	got, isNew, err := s.Msgs.Insert(ctx, env)
	if !model.IsCode(err, model.ErrConflict) || isNew || len(got.Payload) != 0 {
		t.Fatalf("foreign replay exposed message: %+v %v", got, err)
	}
}

func TestOnlyCreatorMayVerify(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	creator := mustCreateAgent(t, s.Agents)
	worker := mustCreateAgent(t, s.Agents)
	other := mustCreateAgent(t, s.Agents)
	task, err := s.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "work"}, "")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Tasks.Claim(ctx, task.TaskID, worker.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	msg, _, err := s.Msgs.Insert(ctx, model.Envelope{MessageID: "result", Sender: worker.AgentID, Recipient: creator.AgentID, Type: "RESULT", TaskID: task.TaskID, Payload: []byte(toJSON(model.ResultPayload{Summary: "done", Status: "success", ClaimID: claimed.ClaimID}))})
	if err != nil {
		t.Fatal(err)
	}
	req := model.VerifyRequest{TargetMessageID: msg.MessageID, Verdict: "verified"}
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, other.AgentID, req); !model.IsCode(err, model.ErrAccessDenied) {
		t.Fatalf("outsider verification: %v", err)
	}
	unchanged, err := s.Tasks.Get(ctx, creator.AgentID, task.TaskID)
	if err != nil || unchanged.Status != model.TaskSubmitted {
		t.Fatalf("task mutated: %+v %v", unchanged, err)
	}
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, creator.AgentID, req); err != nil {
		t.Fatal(err)
	}
}
