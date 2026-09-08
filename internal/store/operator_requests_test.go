package store

import (
	"aichatdeck/internal/model"
	"context"
	"testing"
	"time"
)

func TestOperatorReplyAtomicAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	a := mustCreateAgent(t, s.Agents)
	requests := NewOperatorRequestStore(s.Agents.db)
	record, err := requests.Create(ctx, a.AgentID, model.OperatorRequest{Kind: "request", Title: "question", Details: "details"})
	if err != nil {
		t.Fatal(err)
	}
	if err = requests.SetTelegramMessage(ctx, record.RequestID, 42); err != nil {
		t.Fatal(err)
	}
	// Force a failure after message insertion: all effects must roll back.
	_, err = s.Agents.db.Exec(`CREATE TRIGGER fail_answer BEFORE UPDATE ON operator_requests
 WHEN NEW.status='answered' BEGIN SELECT RAISE(ABORT,'test failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = requests.DeliverReply(ctx, 7, 42, "answer"); err == nil {
		t.Fatal("expected failure")
	}
	msgs, _, err := s.Msgs.List(ctx, MessageFilter{Recipient: a.AgentID}, time.Now())
	if err != nil || len(msgs) != 0 {
		t.Fatalf("partial delivery: %v %v", msgs, err)
	}
	got, err := requests.Get(ctx, record.RequestID)
	if err != nil || got.Status != "sent" {
		t.Fatalf("partial status: %v %v", got, err)
	}
	if _, err = s.Agents.db.Exec("DROP TRIGGER fail_answer"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		recipient, isNew, err := requests.DeliverReply(ctx, 7, 42, "answer")
		if err != nil || recipient != a.AgentID || isNew != (i == 0) {
			t.Fatalf("delivery %d: %s %v %v", i, recipient, isNew, err)
		}
	}
	msgs, _, err = s.Msgs.List(ctx, MessageFilter{Recipient: a.AgentID}, time.Now())
	if err != nil || len(msgs) != 1 {
		t.Fatalf("duplicates: %v %v", msgs, err)
	}
	if err := msgs[0].Validate(); err != nil {
		t.Fatal(err)
	}
	got, err = requests.Get(ctx, record.RequestID)
	if err != nil || got.Status != "answered" || got.Reply != "answer" {
		t.Fatalf("status: %+v %v", got, err)
	}
}
