package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
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
		s.writeErr(w, aerr)
		return
	}
	if aerr := env.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if env.Sender != agentID {
		s.writeErr(w, model.AccessDenied("sender must equal the authenticated agent"))
		return
	}
	if env.Recipient != model.BroadcastRecipient {
		if _, err := s.Agents.Get(r.Context(), env.Recipient); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	if env.TaskID != "" {
		// A task id makes the message part of that task's record. Resolve it
		// through the sender's scope first, then require write-capable project
		// membership for every participant. Otherwise a member could relay a
		// closed task's payload to an outsider through a direct message.
		task, err := s.Tasks.Get(r.Context(), agentID, env.TaskID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if task.ProjectID != nil {
			if err := s.requireProjectWriter(r, *task.ProjectID, agentID); err != nil {
				s.writeErr(w, err)
				return
			}
			if env.Recipient == model.BroadcastRecipient {
				project, err := s.Projects.Get(r.Context(), *task.ProjectID)
				if err != nil {
					s.writeErr(w, err)
					return
				}
				if project.Visibility == model.ProjectClosed {
					s.writeErr(w, model.AccessDenied("a closed-project task cannot broadcast messages"))
					return
				}
			} else {
				if _, err := s.Tasks.Get(r.Context(), env.Recipient, task.TaskID); err != nil {
					s.writeErr(w, model.AccessDenied("recipient may not access this task"))
					return
				}
				if err := s.requireProjectWriter(r, *task.ProjectID, env.Recipient); err != nil {
					s.writeErr(w, model.AccessDenied("recipient may not receive project task messages"))
					return
				}
			}
		}
		if env.Type == "RESULT" {
			if task.Owner == nil || *task.Owner != agentID {
				s.writeErr(w, model.AccessDenied("only the task's current owner may report a RESULT"))
				return
			}
			if env.Recipient != task.CreatedBy {
				s.writeErr(w, model.ValidationError("a task RESULT must be sent to its creator"))
				return
			}
			var result model.ResultPayload
			if err := json.Unmarshal(env.Payload, &result); err != nil {
				s.writeErr(w, model.ValidationError("invalid RESULT payload"))
				return
			}
			for _, artifactID := range result.Artifacts {
				artifact, err := s.Artifacts.Get(r.Context(), agentID, artifactID)
				if err != nil || artifact.TaskID == nil || *artifact.TaskID != task.TaskID || artifact.CreatedBy != agentID {
					s.writeErr(w, model.ValidationError("RESULT artifacts must be created by the task owner and attached to this task"))
					return
				}
			}
		}
	}

	stored, isNew, err := s.Messages.Insert(r.Context(), env)
	if err != nil {
		s.writeErr(w, err)
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
	if types := q.Get("type"); types != "" {
		filter.Types = strings.Split(types, ",")
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
		s.writeErr(w, err)
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
				s.writeErr(w, err)
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
