package store

import (
	"context"
	"testing"
	"time"

	"aichatdeck/internal/model"
)

func TestMessageInsertDeduplicates(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	a := mustCreateAgent(t, stores.Agents)
	b := mustCreateAgent(t, stores.Agents)

	env := model.Envelope{
		MessageID: "dup-1", ProtocolVersion: "1.0", Type: "EVENT",
		Sender: a.AgentID, Recipient: b.AgentID, Priority: "normal",
		Timestamp: time.Now().UTC(),
		Payload:   []byte(`{"event_type":"ping"}`),
	}

	first, isNew1, err := stores.Msgs.Insert(ctx, env)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !isNew1 {
		t.Fatal("expected first insert to be new")
	}

	second, isNew2, err := stores.Msgs.Insert(ctx, env)
	if err != nil {
		t.Fatalf("replay insert: %v", err)
	}
	if isNew2 {
		t.Fatal("expected replay to not be new")
	}
	if first.MessageID != second.MessageID {
		t.Fatalf("expected same stored row, got %s vs %s", first.MessageID, second.MessageID)
	}

	msgs, _, err := stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one stored message despite two inserts, got %d", len(msgs))
	}
}

func TestMessageListFiltersExpired(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	a := mustCreateAgent(t, stores.Agents)
	b := mustCreateAgent(t, stores.Agents)

	expiredTTL := 1
	expired := model.Envelope{
		MessageID: "exp-1", ProtocolVersion: "1.0", Type: "EVENT",
		Sender: a.AgentID, Recipient: b.AgentID, Priority: "normal",
		Timestamp: time.Now().UTC().Add(-1 * time.Hour),
		TTL:       &expiredTTL,
		Payload:   []byte(`{"event_type":"stale"}`),
	}
	fresh := model.Envelope{
		MessageID: "exp-2", ProtocolVersion: "1.0", Type: "EVENT",
		Sender: a.AgentID, Recipient: b.AgentID, Priority: "normal",
		Timestamp: time.Now().UTC(),
		Payload:   []byte(`{"event_type":"fresh"}`),
	}

	if _, _, err := stores.Msgs.Insert(ctx, expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	if _, _, err := stores.Msgs.Insert(ctx, fresh); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}

	msgs, _, err := stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "exp-2" {
		t.Fatalf("expected only the fresh message, got %+v", msgs)
	}
}

func TestMessageListBroadcastVisibleToAll(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	a := mustCreateAgent(t, stores.Agents)
	b := mustCreateAgent(t, stores.Agents)

	env := model.Envelope{
		MessageID: "bcast-1", ProtocolVersion: "1.0", Type: "EVENT",
		Sender: a.AgentID, Recipient: model.BroadcastRecipient, Priority: "normal",
		Timestamp: time.Now().UTC(),
		Payload:   []byte(`{"event_type":"announcement"}`),
	}
	if _, _, err := stores.Msgs.Insert(ctx, env); err != nil {
		t.Fatalf("insert: %v", err)
	}

	msgs, _, err := stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected broadcast visible to b, got %d messages", len(msgs))
	}
}
