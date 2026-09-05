package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// BroadcastRecipient is the literal recipient value for broadcast messages
// (RFC-1100 §3).
const BroadcastRecipient = "broadcast"

// Message types supported this milestone (RFC-1100 §4). PLAN, REQUEST,
// PROPOSAL, HANDOFF, REVIEW and TASK are deferred with Roles/Teams/Projects —
// see the plan's scope cuts.
var supportedTypes = map[string]bool{
	"CLAIM":    true,
	"RESULT":   true,
	"VERIFY":   true,
	"STATUS":   true,
	"ERROR":    true,
	"QUESTION": true,
	"ANSWER":   true,
	"EVENT":    true,
}

var supportedPriorities = map[string]bool{
	"low": true, "normal": true, "high": true, "critical": true,
}

// Envelope is the RFC-1100 §5 message envelope.
type Envelope struct {
	MessageID       string          `json:"message_id"`
	ProtocolVersion string          `json:"protocol_version"`
	Type            string          `json:"type"`
	Sender          string          `json:"sender"`
	Recipient       string          `json:"recipient"`
	Timestamp       time.Time       `json:"timestamp"`
	ConversationID  string          `json:"conversation_id,omitempty"`
	TaskID          string          `json:"task_id,omitempty"`
	Priority        string          `json:"priority"`
	Payload         json.RawMessage `json:"payload"`
	Evidence        []string        `json:"evidence,omitempty"`
	ReplyTo         *string         `json:"reply_to,omitempty"`
	TTL             *int            `json:"ttl,omitempty"` // seconds
}

// Validate checks envelope-level rules (RFC-1100 §5) and dispatches to the
// per-type payload validator (§6.x).
func (e *Envelope) Validate() *APIError {
	if e.MessageID == "" {
		return ValidationError("message_id is required")
	}
	if e.ProtocolVersion == "" {
		return ValidationError("protocol_version is required")
	}
	if !supportedTypes[e.Type] {
		return ValidationError(fmt.Sprintf("unsupported message type %q", e.Type)).
			WithDetails(map[string]string{"reason": "unsupported_message_type"})
	}
	if e.Sender == "" {
		return ValidationError("sender is required")
	}
	if e.Recipient == "" {
		return ValidationError("recipient is required")
	}
	if e.Timestamp.IsZero() {
		return ValidationError("timestamp is required")
	}
	if e.Priority == "" {
		e.Priority = "normal"
	}
	if !supportedPriorities[e.Priority] {
		return ValidationError(fmt.Sprintf("invalid priority %q", e.Priority))
	}
	// ANSWER MUST reference the QUESTION it answers (RFC-1100 §6.4).
	if e.Type == "ANSWER" && (e.ReplyTo == nil || *e.ReplyTo == "") {
		return ValidationError("ANSWER requires reply_to")
	}
	if e.TTL != nil && *e.TTL < 0 {
		return ValidationError("ttl must not be negative")
	}
	for field, v := range map[string]string{"message_id": e.MessageID, "sender": e.Sender, "recipient": e.Recipient} {
		if err := checkText(field, v, MaxShortField); err != nil {
			return err
		}
	}
	// One bound for every payload shape: the per-type checks below only see
	// the fields they know about, and payload is free-form JSON.
	if len(e.Payload) > MaxObjectSize {
		return ValidationError(fmt.Sprintf("payload must be at most %d bytes of JSON", MaxObjectSize))
	}
	return ValidatePayload(e.Type, e.Payload)
}

// Expired reports whether the envelope's ttl (RFC-1100 §10) has elapsed as of now.
func (e *Envelope) Expired(now time.Time) bool {
	if e.TTL == nil {
		return false
	}
	return now.After(e.Timestamp.Add(time.Duration(*e.TTL) * time.Second))
}

// --- Per-type payloads (RFC-1100 §6.x) ---

type ClaimPayload struct {
	ClaimType  string   `json:"claim_type"`
	Statement  string   `json:"statement"`
	Confidence float64  `json:"confidence"`
	Basis      []string `json:"basis,omitempty"`
}

type ResultPayload struct {
	Summary   string         `json:"summary"`
	Artifacts []string       `json:"artifacts,omitempty"`
	Metrics   map[string]any `json:"metrics,omitempty"`
	Status    string         `json:"status"`
}

type QuestionPayload struct {
	Question string         `json:"question"`
	Context  map[string]any `json:"context,omitempty"`
	Blocking bool           `json:"blocking,omitempty"`
}

type AnswerPayload struct {
	Answer     string   `json:"answer"`
	References []string `json:"references,omitempty"`
}

type VerifyPayload struct {
	TargetMessageID string   `json:"target_message_id"`
	Verdict         string   `json:"verdict"`
	Rationale       string   `json:"rationale,omitempty"`
	Evidence        []string `json:"evidence,omitempty"`
}

type StatusPayload struct {
	Subject   string `json:"subject"`
	SubjectID string `json:"subject_id"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
}

type ErrorPayload struct {
	ErrorCode       string `json:"error_code"`
	Message         string `json:"message"`
	Retryable       bool   `json:"retryable"`
	Details         any    `json:"details,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
}

type EventPayload struct {
	EventType string         `json:"event_type"`
	Data      map[string]any `json:"data,omitempty"`
}

// ValidatePayload unmarshals and checks payload against the schema for msgType.
func ValidatePayload(msgType string, raw json.RawMessage) *APIError {
	switch msgType {
	case "CLAIM":
		var p ClaimPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid CLAIM payload: " + err.Error())
		}
		if p.ClaimType != "hypothesis" && p.ClaimType != "solution" && p.ClaimType != "task_claim" {
			return ValidationError("claim_type must be hypothesis, solution or task_claim")
		}
		if p.Statement == "" {
			return ValidationError("statement is required")
		}
		if p.Confidence < 0 || p.Confidence > 1 {
			return ValidationError("confidence must be between 0 and 1")
		}
	case "RESULT":
		var p ResultPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid RESULT payload: " + err.Error())
		}
		if p.Summary == "" {
			return ValidationError("summary is required")
		}
		// A RESULT summary is read into the creating agent's Verify prompt.
		if err := checkText("summary", p.Summary, MaxTextField); err != nil {
			return err
		}
		if p.Status != "success" && p.Status != "partial" && p.Status != "failure" {
			return ValidationError("status must be success, partial or failure")
		}
	case "VERIFY":
		var p VerifyPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid VERIFY payload: " + err.Error())
		}
		if p.TargetMessageID == "" {
			return ValidationError("target_message_id is required")
		}
		if p.Verdict != "verified" && p.Verdict != "rejected" && p.Verdict != "inconclusive" {
			return ValidationError("verdict must be verified, rejected or inconclusive")
		}
	case "STATUS":
		var p StatusPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid STATUS payload: " + err.Error())
		}
		if p.Subject != "agent" && p.Subject != "task" {
			return ValidationError("subject must be agent or task")
		}
		if p.SubjectID == "" {
			return ValidationError("subject_id is required")
		}
		switch p.State {
		case "idle", "working", "blocked", "waiting", "completed", "failed":
		default:
			return ValidationError("invalid state")
		}
	case "ERROR":
		var p ErrorPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid ERROR payload: " + err.Error())
		}
		if p.ErrorCode == "" || p.Message == "" {
			return ValidationError("error_code and message are required")
		}
	case "QUESTION":
		var p QuestionPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid QUESTION payload: " + err.Error())
		}
		if p.Question == "" {
			return ValidationError("question is required")
		}
	case "ANSWER":
		var p AnswerPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid ANSWER payload: " + err.Error())
		}
		if p.Answer == "" {
			return ValidationError("answer is required")
		}
	case "EVENT":
		var p EventPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return ValidationError("invalid EVENT payload: " + err.Error())
		}
		if p.EventType == "" {
			return ValidationError("event_type is required")
		}
	default:
		return ValidationError(fmt.Sprintf("unsupported message type %q", msgType))
	}
	return nil
}
