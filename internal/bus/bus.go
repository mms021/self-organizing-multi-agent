// Package bus is the wakeup-signal pub/sub used by GET /messages long-poll.
// It never carries the message envelope itself — SQLite is always the
// source of truth; a bus message is just "something changed, go re-query."
package bus

import (
	"context"
	"sync"
)

// Bus is the minimal interface cmd/server wires to Redis in production and
// tests satisfy with an in-memory fake — no live Redis needed for `go test`.
type Bus interface {
	Publish(ctx context.Context, channel string) error
	// Subscribe returns a channel of wakeup pings and an unsubscribe func.
	Subscribe(ctx context.Context, channels ...string) (<-chan struct{}, func())
}

// AgentChannel and BroadcastChannel name the two channel kinds used by the
// message inbox (RFC-1100 §3 direct/broadcast).
func AgentChannel(agentID string) string { return "agent:" + agentID }

const BroadcastChannel = "broadcast"

// Fake is an in-memory Bus for tests: Publish fans out to any Subscribe
// callers currently listening on that channel; no history, no persistence.
type Fake struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func NewFake() *Fake { return &Fake{subs: map[string][]chan struct{}{}} }

func (f *Fake) Publish(_ context.Context, channel string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs[channel] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *Fake) Subscribe(_ context.Context, channels ...string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	for _, c := range channels {
		f.subs[c] = append(f.subs[c], ch)
	}
	f.mu.Unlock()

	unsub := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range channels {
			list := f.subs[c]
			for i, sub := range list {
				if sub == ch {
					f.subs[c] = append(list[:i], list[i+1:]...)
					break
				}
			}
		}
	}
	return ch, unsub
}
