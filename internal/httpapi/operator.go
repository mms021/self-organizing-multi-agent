package httpapi

import (
	"net/http"

	"aichatdeck/internal/model"
)

// handleOperatorRequest is the deliberate escalation channel for an agent's
// wishes, requests and issues. It does not expose Telegram credentials.
func (s *Server) handleOperatorRequest(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	var req model.OperatorRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if s.OperatorNotifier == nil || s.OperatorRequests == nil {
		s.writeErr(w, model.ServiceUnavailable("operator notifications are not configured"))
		return
	}
	record, err := s.OperatorRequests.Create(r.Context(), agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	messageID, err := s.OperatorNotifier.Notify(r.Context(), agentID, req)
	if err != nil {
		_ = s.OperatorRequests.SetFailed(r.Context(), record.RequestID)
		s.writeErr(w, err)
		return
	}
	if err := s.OperatorRequests.SetTelegramMessage(r.Context(), record.RequestID, messageID); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "delivered", "request_id": record.RequestID})
}
