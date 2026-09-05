package httpapi_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/db"
	"aichatdeck/internal/httpapi"
	"aichatdeck/internal/store"
)

// newBreakableServer hands back the underlying DB so a test can close it and
// force a genuine internal failure.
func newBreakableServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "errors.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	srv := httptest.NewServer(httpapi.NewRouter(&httpapi.Server{
		Agents:      store.NewAgentStore(sqlDB),
		Credentials: store.NewCredentialStore(sqlDB),
		Tasks:       store.NewTaskStore(sqlDB),
		Messages:    store.NewMessageStore(sqlDB),
		Knowledge:   store.NewKnowledgeStore(sqlDB),
		Artifacts:   store.NewArtifactStore(sqlDB),
		Projects:    store.NewProjectStore(sqlDB),
		Bus:         bus.NewFake(),
		DB:          sqlDB,
		// Logger stays nil: the failure is expected, no need to print it.
	}))
	t.Cleanup(srv.Close)
	return srv, sqlDB
}

// Internal failures must not hand the client server-side detail (SQL text,
// file paths) — RFC-1600 §2. The client gets a bare 500 plus a trace_id;
// the detail goes to the server log.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	srv, sqlDB := newBreakableServer(t)
	c := register(t, srv.URL)

	// Close the database out from under the server so the next query fails
	// with a driver-level error.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	status, body := c.do(http.MethodGet, "/tasks", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500 once the DB is unusable, got %d: %v", status, body)
	}
	msg, _ := body["message"].(string)
	if msg != "internal error" {
		t.Errorf("expected an opaque message, got %q", msg)
	}
	for _, leak := range []string{"sql", "sqlite", "database", ".db", "/"} {
		if strings.Contains(strings.ToLower(msg), leak) {
			t.Errorf("internal detail %q leaked to the client: %q", leak, msg)
		}
	}
	if trace, _ := body["trace_id"].(string); trace == "" {
		t.Error("a 500 must carry a trace_id so the client can quote it")
	}
}

// Unauthenticated access to a protected endpoint is access_denied, not a 500
// and not a silent pass-through.
func TestProtectedEndpointsRequireCredential(t *testing.T) {
	srv := newTestServer(t)
	anon := &client{t: t, base: srv.URL}

	for _, path := range []string{"/tasks", "/messages", "/memory/search", "/discovery/projects"} {
		status, body := anon.do(http.MethodGet, path, nil)
		if status != http.StatusForbidden {
			t.Errorf("%s without a credential: expected 403, got %d", path, status)
		}
		if code, _ := body["error_code"].(string); code != "access_denied" {
			t.Errorf("%s: expected access_denied, got %q", path, code)
		}
	}
}

// Malformed and unknown-field bodies are validation errors, never 500s.
func TestBadRequestBodiesAreValidationErrors(t *testing.T) {
	srv := newTestServer(t)
	c := register(t, srv.URL)

	cases := []struct {
		name, path string
		body       any
	}{
		{"unknown field", "/tasks", map[string]any{"objective": "x", "not_a_field": 1}},
		{"missing objective", "/tasks", map[string]any{}},
		{"bad category", "/memory/entries", map[string]any{"category": "gossip", "content": map[string]any{"a": 1}}},
		{"empty content", "/memory/entries", map[string]any{"category": "lesson", "content": map[string]any{}}},
		{"artifact without checksum", "/artifacts", map[string]any{"type": "file", "uri": "file:///x"}},
		{"closed project", "/projects", map[string]any{"name": "p", "visibility": "closed"}},
	}
	for _, tc := range cases {
		status, body := c.do(http.MethodPost, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %v", tc.name, status, body)
			continue
		}
		if code, _ := body["error_code"].(string); code != "validation_error" {
			t.Errorf("%s: expected validation_error, got %q", tc.name, code)
		}
	}
}

// A message whose sender is not the authenticated agent must be refused:
// otherwise any agent could forge messages from any other.
func TestSenderSpoofingRefused(t *testing.T) {
	srv := newTestServer(t)
	a := register(t, srv.URL)
	b := register(t, srv.URL)

	status, body := a.do(http.MethodPost, "/messages", map[string]any{
		"message_id":       "spoof-1",
		"protocol_version": "1.0",
		"type":             "EVENT",
		"sender":           mustAgentID(t, b), // not the caller
		"recipient":        "broadcast",
		"timestamp":        "2026-01-01T00:00:00Z",
		"priority":         "normal",
		"payload":          map[string]any{"event_type": "impersonation"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 for a spoofed sender, got %d: %v", status, body)
	}
}
