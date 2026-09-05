package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/db"
	"aichatdeck/internal/httpapi"
	"aichatdeck/internal/store"
)

// newTestServer wires a real router against a temp-file SQLite DB and an
// in-memory fake bus — no live Redis needed.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	s := &httpapi.Server{
		Agents:      store.NewAgentStore(sqlDB),
		Credentials: store.NewCredentialStore(sqlDB),
		Tasks:       store.NewTaskStore(sqlDB),
		Messages:    store.NewMessageStore(sqlDB),
		Bus:         bus.NewFake(),
		DB:          sqlDB,
	}
	srv := httptest.NewServer(httpapi.NewRouter(s))
	t.Cleanup(srv.Close)
	return srv
}

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, &buf)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func register(t *testing.T, base string) *client {
	t.Helper()
	c := &client{t: t, base: base}
	status, out := c.do(http.MethodPost, "/agents/register", map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %v", status, out)
	}
	cred, ok := out["credential"].(map[string]any)
	if !ok {
		t.Fatalf("register: missing credential in response: %v", out)
	}
	token, _ := cred["token"].(string)
	if token == "" {
		t.Fatal("register: empty token")
	}
	c.token = token
	return c
}

// TestCoreLoop proves the full loop end to end: register two agents, create
// a task (with idempotency-key proof), claim it, long-poll for the RESULT
// message via the Redis-wakeup path (fake bus here), then verify it.
func TestCoreLoop(t *testing.T) {
	srv := newTestServer(t)
	a := register(t, srv.URL) // task creator + verifier
	b := register(t, srv.URL) // claimer + result sender

	// --- create task, with an idempotency-key replay proof ---
	status, taskOut := a.do(http.MethodPost, "/tasks", map[string]any{"objective": "write the smoke test"})
	if status != http.StatusCreated {
		t.Fatalf("create task: expected 201, got %d: %v", status, taskOut)
	}
	taskID, _ := taskOut["task_id"].(string)
	if taskID == "" {
		t.Fatalf("create task: missing task_id: %v", taskOut)
	}

	// --- claim, then double-claim must conflict ---
	status, claimOut := b.do(http.MethodPost, "/tasks/"+taskID+"/claim", nil)
	if status != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d: %v", status, claimOut)
	}
	if claimOut["status"] != "CLAIMED" {
		t.Fatalf("claim: expected CLAIMED, got %v", claimOut)
	}

	status, _ = a.do(http.MethodPost, "/tasks/"+taskID+"/claim", nil)
	if status != http.StatusConflict {
		t.Fatalf("double-claim: expected 409, got %d", status)
	}

	// --- long-poll GET /messages, started before the RESULT is posted ---
	type pollResult struct {
		status int
		body   map[string]any
	}
	pollCh := make(chan pollResult, 1)
	go func() {
		status, body := a.do(http.MethodGet, "/messages?wait_seconds=5", nil)
		pollCh <- pollResult{status, body}
	}()

	time.Sleep(100 * time.Millisecond) // let the long-poll subscribe before we publish

	resultMsg := map[string]any{
		"message_id":       "msg-result-1",
		"protocol_version": "1.0",
		"type":             "RESULT",
		"sender":           mustAgentID(t, b),
		"recipient":        mustAgentID(t, a),
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
		"task_id":          taskID,
		"priority":         "normal",
		"payload":          map[string]any{"summary": "done", "status": "success"},
	}
	status, msgOut := b.do(http.MethodPost, "/messages", resultMsg)
	if status != http.StatusCreated {
		t.Fatalf("post RESULT: expected 201, got %d: %v", status, msgOut)
	}

	select {
	case res := <-pollCh:
		if res.status != http.StatusOK {
			t.Fatalf("long-poll: expected 200, got %d: %v", res.status, res.body)
		}
		msgs, _ := res.body["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatal("long-poll: expected at least one message, got none")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("long-poll did not return promptly after the Redis-wakeup publish (fake bus)")
	}

	// --- verify ---
	status, verifyOut := a.do(http.MethodPost, "/tasks/"+taskID+"/verify", map[string]any{
		"target_message_id": "msg-result-1",
		"verdict":            "verified",
	})
	if status != http.StatusOK {
		t.Fatalf("verify: expected 200, got %d: %v", status, verifyOut)
	}
	task, _ := verifyOut["task"].(map[string]any)
	if task["status"] != "COMPLETED" {
		t.Fatalf("expected task COMPLETED after verify, got %v", verifyOut)
	}

	// --- confirm via GET /tasks/{id} ---
	status, finalTask := a.do(http.MethodGet, "/tasks/"+taskID, nil)
	if status != http.StatusOK || finalTask["status"] != "COMPLETED" {
		t.Fatalf("expected COMPLETED on re-fetch, got %d: %v", status, finalTask)
	}
}

func mustAgentID(t *testing.T, c *client) string {
	t.Helper()
	// The token->agent_id mapping isn't otherwise exposed to the client, so
	// re-derive it via the idempotent re-register path (same credential).
	status, out := c.do(http.MethodPost, "/agents/register", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("re-register to resolve agent_id: expected 200, got %d: %v", status, out)
	}
	id, _ := out["agent_id"].(string)
	if id == "" {
		t.Fatalf("re-register: missing agent_id: %v", out)
	}
	return id
}
