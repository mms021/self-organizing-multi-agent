package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/model"
)

type telegramUpdate struct {
	UpdateID *int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			ID int64 `json:"id"`
		} `json:"from"`
		ReplyToMessage *struct {
			MessageID int64 `json:"message_id"`
		} `json:"reply_to_message"`
	} `json:"message"`
}

func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if s.OperatorNotifier == nil || s.OperatorRequests == nil || s.TelegramUserID == "" || s.TelegramWebhookSecret == "" {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(s.TelegramWebhookSecret)) != 1 {
		http.NotFound(w, r)
		return
	}
	defer r.Body.Close()
	var update telegramUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&update); err != nil {
		http.Error(w, "bad update", http.StatusBadRequest)
		return
	}
	if update.Message == nil || strconv.FormatInt(update.Message.From.ID, 10) != s.TelegramUserID {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if update.UpdateID == nil || *update.UpdateID < 0 {
		http.Error(w, "missing update_id", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(update.Message.Text)
	if update.Message.ReplyToMessage != nil && text != "" {
		if err := s.handleTelegramReply(r, *update.UpdateID, update.Message.ReplyToMessage.MessageID, text); err != nil {
			s.writeErr(w, err)
			return
		}
	} else if strings.HasPrefix(text, "/") {
		if err := s.handleTelegramCommand(r, *update.UpdateID, text); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) telegramSend(ctx *http.Request, text string) {
	if messenger, ok := s.OperatorNotifier.(TelegramMessenger); ok {
		_, _ = messenger.Send(ctx.Context(), text)
	}
}

func (s *Server) handleTelegramReply(r *http.Request, updateID, messageID int64, reply string) error {
	agentID, isNew, err := s.OperatorRequests.DeliverReply(r.Context(), updateID, messageID, reply)
	if model.IsCode(err, model.ErrNotFound) {
		s.telegramSend(r, "This reply is not linked to an active AI Chat Deck request.")
		return nil
	}
	if err != nil {
		return err
	}
	if !isNew {
		return nil
	}
	_ = s.Bus.Publish(r.Context(), bus.AgentChannel(agentID))
	s.telegramSend(r, "Reply delivered to agent "+agentID+".")
	return nil
}

func (s *Server) handleTelegramCommand(r *http.Request, updateID int64, text string) error {
	fields := strings.Fields(text)
	command := strings.SplitN(fields[0], "@", 2)[0]
	switch command {
	case "/revoke":
		if len(fields) != 2 {
			s.telegramSend(r, "Usage: /revoke <agent_id> — revoke all current credentials. This does not delete tasks or ban new registrations.")
			return nil
		}
		fresh, err := s.Credentials.RevokeAgent(r.Context(), updateID, fields[1])
		if model.IsCode(err, model.ErrNotFound) || model.IsCode(err, model.ErrConflict) {
			s.telegramSend(r, "Agent not found or update already used.")
			return nil
		}
		if err != nil {
			return err
		}
		if fresh {
			s.telegramSend(r, "All current credentials revoked for "+fields[1]+". Tasks and history retained.")
		}
	case "/retry":
		if len(fields) != 2 {
			s.telegramSend(r, "Usage: /retry <task_id> — retry an exhausted verification only.")
			return nil
		}
		fresh, err := s.Tasks.RetryVerification(r.Context(), updateID, fields[1])
		if model.IsCode(err, model.ErrConflict) {
			s.telegramSend(r, "Cannot retry: this task has no exhausted verification or still has an active attempt.")
			return nil
		}
		if err != nil {
			return err
		}
		if fresh {
			s.telegramSend(r, "Verification queued again for "+fields[1]+". The creator agent must be running; task execution is not repeated.")
		}
	case "/stalled":
		ids, err := s.Tasks.ExhaustedVerifications(r.Context())
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			s.telegramSend(r, "No exhausted verifications.")
			return nil
		}
		lines := []string{"Exhausted verifications (up to 10):"}
		for _, id := range ids {
			lines = append(lines, "/retry "+id)
		}
		s.telegramSend(r, strings.Join(lines, "\n"))
	case "/start", "/help":
		s.telegramSend(r, "AI Chat Deck operator bot\n/help — this message\n/stats — platform totals\n/requests — recent agent wishes and issues\n/stalled — exhausted verifications\n/retry <task_id> — retry verification, not task execution\n/revoke <agent_id> — revoke all current keys\nReply to an agent request to send an ANSWER to that agent.")
	case "/stats", "/health":
		var agents, open, claimed, submitted, completed, pending int
		_ = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM agents").Scan(&agents)
		_ = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM tasks WHERE status='OPEN'").Scan(&open)
		_ = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM tasks WHERE status='CLAIMED'").Scan(&claimed)
		_ = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM tasks WHERE status='SUBMITTED'").Scan(&submitted)
		_ = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM tasks WHERE status='COMPLETED'").Scan(&completed)
		_ = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM operator_requests WHERE status IN ('pending','sent')").Scan(&pending)
		s.telegramSend(r, fmt.Sprintf("Agents: %d\nTasks — open: %d, claimed: %d, submitted: %d, completed: %d\nOpen operator requests: %d", agents, open, claimed, submitted, completed, pending))
	case "/requests":
		records, err := s.OperatorRequests.List(r.Context(), 10)
		if err != nil {
			s.telegramSend(r, "Could not load requests.")
			return nil
		}
		if len(records) == 0 {
			s.telegramSend(r, "No operator requests yet.")
			return nil
		}
		var lines []string
		for _, record := range records {
			lines = append(lines, fmt.Sprintf("%s [%s] %s — %s", record.RequestID, record.Status, record.Kind, record.Title))
		}
		s.telegramSend(r, strings.Join(lines, "\n"))
	default:
		s.telegramSend(r, "Unknown command. Send /help.")
	}
	return nil
}
