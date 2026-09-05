package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/model"
	"aichatdeck/internal/store"
)

const maxWaitSeconds = 30

// handlePostMessage validates and persists an envelope (RFC-1100 §5). Sender
// spoofing is rejected here — the envelope's sender MUST equal the
// authenticated caller.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	var env model.Envelope
	if aerr := decodeJSON(r, &env); aerr != nil {
		writeErr(w, aerr)
		return
	}
	if aerr := env.Validate(); aerr != nil {
		writeErr(w, aerr)
		return
	}
	if env.Sender != agentID {
		writeErr(w, model.AccessDenied("sender must equal the authenticated agent"))
		return
	}
	if env.Recipient != model.BroadcastRecipient {
		if _, err := s.Agents.Get(r.Context(), env.Recipient); err != nil {
			writeErr(w, err)
			return
		}
	}

	stored, isNew, err := s.Messages.Insert(r.Context(), env)
	if err != nil {
		writeErr(w, err)
		return
	}
	if isNew {
		channel := bus.BroadcastChannel
		if stored.Recipient != model.BroadcastRecipient {
			channel = bus.AgentChannel(stored.Recipient)
		}
		_ = s.Bus.Publish(r.Context(), channel) // best-effort wakeup; SQLite is the source of truth either way
	}

	status := http.StatusCreated
	if !isNew {
		status = http.StatusOK
	}
	writeJSON(w, status, stored)
}

// handleListMessages is GET /messages: an immediate poll of SQLite, or — if
// empty and wait_seconds is set — a long-poll woken by the Redis pub/sub
// ping published in handlePostMessage.
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	q := r.URL.Query()

	filter := store.MessageFilter{
		Recipient: agentID,
		TaskID:    q.Get("task_id"),
		Cursor:    q.Get("cursor"),
	}
	waitSeconds := 0
	if v := q.Get("wait_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			waitSeconds = n
		}
	}
	if waitSeconds > maxWaitSeconds {
		waitSeconds = maxWaitSeconds
	}

	msgs, next, err := s.Messages.List(r.Context(), filter, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}

	if len(msgs) == 0 && waitSeconds > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(waitSeconds)*time.Second)
		defer cancel()
		ch, unsub := s.Bus.Subscribe(ctx, bus.AgentChannel(agentID), bus.BroadcastChannel)
		defer unsub()
		select {
		case <-ch:
			msgs, next, err = s.Messages.List(r.Context(), filter, time.Now().UTC())
			if err != nil {
				writeErr(w, err)
				return
			}
		case <-ctx.Done():
			// Timeout (or client disconnect): return the empty result as-is.
		}
	}

	if msgs == nil {
		msgs = []model.Envelope{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "next_cursor": next})
}
