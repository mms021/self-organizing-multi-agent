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
		Members:     store.NewMembershipStore(sqlDB),
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

// Every route that acts on behalf of an agent must be behind RequireAuth.
// Without it a handler would read an empty agent_id from the context and
// silently create records owned by nobody, so this enumerates the whole
// protected surface rather than a sample.
func TestProtectedEndpointsRequireCredential(t *testing.T) {
	srv := newTestServer(t)
	anon := &client{t: t, base: srv.URL}

	protected := []struct{ method, path string }{
		{http.MethodGet, "/agents/agent-x"},
		{http.MethodGet, "/tasks"},
		{http.MethodPost, "/tasks"},
		{http.MethodGet, "/tasks/task-x"},
		{http.MethodPost, "/tasks/task-x/claim"},
		{http.MethodPost, "/tasks/task-x/verify"},
		{http.MethodGet, "/messages"},
		{http.MethodPost, "/messages"},
		{http.MethodGet, "/memory/search"},
		{http.MethodPost, "/memory/entries"},
		{http.MethodGet, "/memory/entries/knowledge-x"},
		{http.MethodPost, "/memory/entries/knowledge-x/review"},
		{http.MethodPost, "/artifacts"},
		{http.MethodGet, "/artifacts/artifact-x"},
		{http.MethodPost, "/projects"},
		{http.MethodGet, "/projects/project-x"},
		{http.MethodGet, "/discovery/projects"},
	}
	for _, ep := range protected {
		status, body := anon.do(ep.method, ep.path, nil)
		if status != http.StatusForbidden {
			t.Errorf("%s %s without a credential: expected 403, got %d", ep.method, ep.path, status)
			continue
		}
		if code, _ := body["error_code"].(string); code != "access_denied" {
			t.Errorf("%s %s: expected access_denied, got %q", ep.method, ep.path, code)
		}
	}
}

// The public surface must stay reachable without a credential — an agent has
// to read the manifest before it can register (RFC-1400 §2).
func TestPublicEndpointsNeedNoCredential(t *testing.T) {
	srv := newTestServer(t)
	anon := &client{t: t, base: srv.URL}

	for _, path := range []string{"/manifest", "/discovery", "/health", "/donate"} {
		status, _ := anon.do(http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Errorf("%s: expected 200 without a credential, got %d", path, status)
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
		{"artifact without checksum", "/artifacts", map[string]any{"type": "file", "uri": "https://example.com/x"}},
		{"executable artifact", "/artifacts", map[string]any{"type": "file", "uri": "https://example.com/run.exe", "checksum": "sha256:fake"}},
		{"disguised executable artifact", "/artifacts", map[string]any{"type": "log", "uri": "https://example.com/report.exe.txt", "checksum": "sha256:fake"}},
		{"local artifact", "/artifacts", map[string]any{"type": "file", "uri": "file:///tmp/report.txt", "checksum": "sha256:fake"}},
		{"bad visibility", "/projects", map[string]any{"name": "p", "visibility": "secret"}},
		{"listed on an open project", "/projects", map[string]any{"name": "p", "listed": false}},
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

// Oversized input must be rejected as a validation error, at the transport cap
// and at the field caps — an agent cannot make the server (or another agent's
// prompt) swallow an arbitrary amount of text.
func TestOversizedInputIsRejected(t *testing.T) {
	srv := newTestServer(t)
	c := register(t, srv.URL)

	cases := []struct {
		name, path string
		body       any
	}{
		{"body over the transport cap", "/tasks", map[string]any{"objective": strings.Repeat("a", 2<<20)}},
		{"objective over the field cap", "/tasks", map[string]any{"objective": strings.Repeat("a", 5000)}},
		{"too many success criteria", "/tasks", map[string]any{
			"objective": "x", "success_criteria": make([]string, 100)}},
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

// Registration is the only unauthenticated write: unbounded it is a faucet for
// agents and credentials, so it is rate limited per client IP.
func TestRegistrationIsRateLimited(t *testing.T) {
	srv := newTestServer(t)
	anon := &client{t: t, base: srv.URL}

	limited := false
	for i := 0; i < 40 && !limited; i++ {
		status, body := anon.do(http.MethodPost, "/agents/register", registerBody())
		switch status {
		case http.StatusCreated:
		case http.StatusTooManyRequests:
			if code, _ := body["error_code"].(string); code != "rate_limited" {
				t.Fatalf("expected rate_limited, got %q", code)
			}
			limited = true
		default:
			t.Fatalf("register #%d: unexpected status %d: %v", i, status, body)
		}
	}
	if !limited {
		t.Error("registration accepted 40 requests in a burst without limiting")
	}
}
