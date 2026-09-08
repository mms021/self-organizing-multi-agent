package model

import "time"

// VerifyRequest is the POST /tasks/{task_id}/verify body (RFC-1100 §6.7).
type VerifyRequest struct {
	AttemptID       string   `json:"attempt_id,omitempty"`
	TargetMessageID string   `json:"target_message_id"`
	Verdict         string   `json:"verdict"`
	Rationale       string   `json:"rationale,omitempty"`
	Evidence        []string `json:"evidence,omitempty"`
}

func (r *VerifyRequest) Validate() *APIError {
	if err := checkText("attempt_id", r.AttemptID, MaxShortField); err != nil {
		return err
	}
	if r.TargetMessageID == "" {
		return ValidationError("target_message_id is required")
	}
	switch r.Verdict {
	case "verified", "rejected", "inconclusive":
	default:
		return ValidationError("verdict must be verified, rejected or inconclusive")
	}
	if err := checkText("target_message_id", r.TargetMessageID, MaxShortField); err != nil {
		return err
	}
	if err := checkText("rationale", r.Rationale, MaxTextField); err != nil {
		return err
	}
	return checkList("evidence", r.Evidence, MaxListItems, MaxShortField)
}

// Verification is the RFC-1800 persisted Verification shape.
type Verification struct {
	VerificationID  string    `json:"verification_id"`
	SchemaVersion   string    `json:"schema_version"`
	TaskID          string    `json:"task_id"`
	VerifierID      string    `json:"verifier_id"`
	TargetMessageID string    `json:"target_message_id"`
	Verdict         string    `json:"verdict"`
	Rationale       string    `json:"rationale,omitempty"`
	Evidence        []string  `json:"evidence,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
}

// VerifyResponse is the POST /tasks/{task_id}/verify response.
type VerifyResponse struct {
	Task         Task         `json:"task"`
	Verification Verification `json:"verification"`
}

// VerificationJob is a durable leased attempt, independent of inbox cursors.
type VerificationJob struct {
	AttemptID  string    `json:"attempt_id"`
	Attempts   int       `json:"attempts"`
	LeaseUntil time.Time `json:"lease_until"`
	Task       Task      `json:"task"`
	Result     Envelope  `json:"result"`
}
