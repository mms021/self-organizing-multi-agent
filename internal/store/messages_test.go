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

// A polling inbox must never see the same message twice: the returned cursor
// always resumes past the last row, even when there is no further page.
func TestMessageListCursorAdvancesOnPartialPage(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	a := mustCreateAgent(t, stores.Agents)
	b := mustCreateAgent(t, stores.Agents)

	insert := func(id string) {
		t.Helper()
		_, _, err := stores.Msgs.Insert(ctx, model.Envelope{
			MessageID: id, ProtocolVersion: "1.0", Type: "EVENT",
			Sender: a.AgentID, Recipient: b.AgentID, Priority: "normal",
			Timestamp: time.Now().UTC(),
			Payload:   []byte(`{"event_type":"ping"}`),
		})
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	insert("cur-1")
	msgs, cursor, err := stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	if len(msgs) != 1 || cursor == "" {
		t.Fatalf("expected 1 message and a non-empty cursor, got %d msgs cursor=%q", len(msgs), cursor)
	}

	// Same cursor, nothing new: must come back empty, not replay cur-1.
	msgs, cursor2, err := stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID, Cursor: cursor}, time.Now().UTC())
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no replay, got %d messages", len(msgs))
	}
	if cursor2 != cursor {
		t.Fatalf("empty page should keep the caller's position, got %q want %q", cursor2, cursor)
	}

	insert("cur-2")
	msgs, _, err = stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID, Cursor: cursor2}, time.Now().UTC())
	if err != nil {
		t.Fatalf("third list: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "cur-2" {
		t.Fatalf("expected only the new message, got %+v", msgs)
	}
}

// Regression: messages that land in the same second must all be delivered.
// Paginating on received_at (second granularity) with a message_id tie-break
// ordered same-second messages by random uuid, so a cursor could step past a
// message and never return it.
func TestMessageListNoSkipWithinSameSecond(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	a := mustCreateAgent(t, stores.Agents)
	b := mustCreateAgent(t, stores.Agents)

	sameInstant := time.Now().UTC()
	insert := func(id string) {
		t.Helper()
		_, _, err := stores.Msgs.Insert(ctx, model.Envelope{
			MessageID: id, ProtocolVersion: "1.0", Type: "EVENT",
			Sender: a.AgentID, Recipient: b.AgentID, Priority: "normal",
			Timestamp: sameInstant,
			Payload:   []byte(`{"event_type":"ping"}`),
		})
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	// The inbox drains what has arrived so far...
	insert("mmm-consumed-first")
	msgs, cursor, err := stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected the first message, got %d", len(msgs))
	}

	// ...then a message arrives in the same second whose id sorts *before* the
	// one already consumed. Ordering by (received_at, message_id) would place
	// it behind the cursor and drop it permanently.
	insert("aaa-arrives-later")

	msgs, _, err = stores.Msgs.List(ctx, MessageFilter{Recipient: b.AgentID, Cursor: cursor}, time.Now().UTC())
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "aaa-arrives-later" {
		t.Fatalf("later message with a lower id was skipped by the cursor; got %+v", msgs)
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
