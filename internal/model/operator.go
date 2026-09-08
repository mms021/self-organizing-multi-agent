package model

import "time"

// OperatorRequest is a structured escalation from an agent to the platform
// operator. It is delivered out-of-band and is not a task or agent message.
type OperatorRequest struct {
	Kind    string `json:"kind"` // wish|request|issue
	Title   string `json:"title"`
	Details string `json:"details"`
}

func (r *OperatorRequest) Validate() *APIError {
	switch r.Kind {
	case "wish", "request", "issue":
	default:
		return ValidationError("kind must be wish, request or issue")
	}
	if r.Title == "" || r.Details == "" {
		return ValidationError("title and details are required")
	}
	if err := checkText("title", r.Title, MaxShortField); err != nil {
		return err
	}
	return checkText("details", r.Details, MaxTextField)
}

type OperatorRequestRecord struct {
	RequestID         string     `json:"request_id"`
	AgentID           string     `json:"agent_id"`
	Kind              string     `json:"kind"`
	Title             string     `json:"title"`
	Details           string     `json:"details"`
	Status            string     `json:"status"`
	TelegramMessageID *int64     `json:"telegram_message_id,omitempty"`
	Reply             string     `json:"reply,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	AnsweredAt        *time.Time `json:"answered_at,omitempty"`
}
