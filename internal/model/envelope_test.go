package model

import (
	"encoding/json"
	"testing"
	"time"
)

func baseEnvelope(msgType string, payload string) Envelope {
	return Envelope{
		MessageID:       "msg-1",
		ProtocolVersion: "1.0",
		Type:            msgType,
		Sender:          "agent-a",
		Recipient:       "agent-b",
		Timestamp:       time.Now().UTC(),
		Priority:        "normal",
		Payload:         json.RawMessage(payload),
	}
}

func TestEnvelopeValidate_Valid(t *testing.T) {
	replyTo := "msg-0"
	cases := []Envelope{
		baseEnvelope("CLAIM", `{"claim_type":"hypothesis","statement":"x","confidence":0.5}`),
		baseEnvelope("RESULT", `{"summary":"done","status":"success"}`),
		baseEnvelope("VERIFY", `{"target_message_id":"msg-2","verdict":"verified"}`),
		baseEnvelope("STATUS", `{"subject":"task","subject_id":"task-1","state":"working"}`),
		baseEnvelope("ERROR", `{"error_code":"not_found","message":"nope"}`),
		baseEnvelope("QUESTION", `{"question":"why?"}`),
		baseEnvelope("EVENT", `{"event_type":"team_formed"}`),
	}
	for _, env := range cases {
		if err := env.Validate(); err != nil {
			t.Errorf("type %s: expected valid, got %v", env.Type, err)
		}
	}

	answer := baseEnvelope("ANSWER", `{"answer":"because"}`)
	answer.ReplyTo = &replyTo
	if err := answer.Validate(); err != nil {
		t.Errorf("ANSWER: expected valid, got %v", err)
	}
}

func TestEnvelopeValidate_Invalid(t *testing.T) {
	t.Run("missing message_id", func(t *testing.T) {
		env := baseEnvelope("EVENT", `{"event_type":"x"}`)
		env.MessageID = ""
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("unsupported type", func(t *testing.T) {
		env := baseEnvelope("HANDOFF", `{}`)
		err := env.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if err.ErrorCode != ErrValidationError {
			t.Errorf("expected validation_error, got %s", err.ErrorCode)
		}
	})

	t.Run("invalid priority", func(t *testing.T) {
		env := baseEnvelope("EVENT", `{"event_type":"x"}`)
		env.Priority = "urgent"
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("ANSWER without reply_to", func(t *testing.T) {
		env := baseEnvelope("ANSWER", `{"answer":"because"}`)
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("CLAIM invalid confidence", func(t *testing.T) {
		env := baseEnvelope("CLAIM", `{"claim_type":"hypothesis","statement":"x","confidence":2}`)
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("RESULT invalid status", func(t *testing.T) {
		env := baseEnvelope("RESULT", `{"summary":"done","status":"maybe"}`)
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("VERIFY invalid verdict", func(t *testing.T) {
		env := baseEnvelope("VERIFY", `{"target_message_id":"msg-2","verdict":"maybe"}`)
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("STATUS invalid state", func(t *testing.T) {
		env := baseEnvelope("STATUS", `{"subject":"task","subject_id":"task-1","state":"confused"}`)
		if err := env.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestEnvelopeExpired(t *testing.T) {
	ttl := 60
	env := Envelope{Timestamp: time.Now().Add(-2 * time.Minute), TTL: &ttl}
	if !env.Expired(time.Now()) {
		t.Error("expected expired")
	}

	env2 := Envelope{Timestamp: time.Now(), TTL: &ttl}
	if env2.Expired(time.Now()) {
		t.Error("expected not expired")
	}

	env3 := Envelope{Timestamp: time.Now().Add(-1 * time.Hour), TTL: nil}
	if env3.Expired(time.Now()) {
		t.Error("nil ttl should never expire")
	}
}
