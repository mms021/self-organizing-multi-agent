package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aichatdeck/internal/agent"
	"aichatdeck/internal/bus"
	"aichatdeck/internal/db"
	"aichatdeck/internal/httpapi"
	"aichatdeck/internal/model"
	"aichatdeck/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	a, b := mcp.NewInMemoryTransports()
	ss, err := s.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}
func call(t *testing.T, c *mcp.ClientSession, name string, args any) map[string]any {
	t.Helper()
	out, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || out.IsError {
		t.Fatalf("%s: %+v %v", name, out, err)
	}
	data, _ := json.Marshal(out.StructuredContent)
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestMCPCoreLoopUsesPlatformAuthorization(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	credentials := store.NewCredentialStore(database)
	h := httptest.NewServer(httpapi.NewRouter(&httpapi.Server{DB: database, Agents: store.NewAgentStore(database), Credentials: credentials, Tasks: store.NewTaskStore(database), Messages: store.NewMessageStore(database), Knowledge: store.NewKnowledgeStore(database), Artifacts: store.NewArtifactStore(database), Projects: store.NewProjectStore(database), Members: store.NewMembershipStore(database), Bus: bus.NewFake()}))
	defer h.Close()
	ctx := context.Background()
	creator, worker := agent.NewClient(h.URL), agent.NewClient(h.URL)
	for _, c := range []*agent.Client{creator, worker} {
		if err := c.Register(ctx, model.RegisterRequest{Capabilities: []model.Capability{{Name: "testing"}}}); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := New(h.URL, creator.AgentID, creator.Token)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := New(h.URL, worker.AgentID, worker.Token)
	if err != nil {
		t.Fatal(err)
	}
	c, w := connect(t, cs), connect(t, ws)
	list, err := w.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 13 {
		t.Fatalf("tool count=%d", len(list.Tools))
	}
	for _, tool := range list.Tools {
		if strings.Contains(tool.Name, "revoke") || strings.Contains(tool.Name, "credential") {
			t.Fatal("administrative tool exposed")
		}
	}
	task := call(t, c, "create_task", map[string]any{"objective": "MCP task"})
	id := task["task_id"].(string)
	page := call(t, w, "list_tasks", map[string]any{"status": "OPEN", "limit": 1})
	if len(page["tasks"].([]any)) != 1 {
		t.Fatalf("missing task: %+v", page)
	}
	claim := call(t, w, "claim_task", map[string]any{"task_id": id})
	call(t, w, "heartbeat_task", map[string]any{"task_id": id, "claim_id": claim["claim_id"]})
	args := map[string]any{"task_id": id, "claim_id": claim["claim_id"], "message_id": "mcp-result", "summary": "done", "status": "success"}
	result := call(t, w, "submit_result", args)
	if result["sender"] != worker.AgentID || result["recipient"] != creator.AgentID {
		t.Fatal("wrong result identity")
	}
	call(t, w, "submit_result", args) // HTTP message dedup is retained
	inbox := call(t, c, "get_messages", map[string]any{"types": []string{"RESULT"}})
	if len(inbox["messages"].([]any)) != 1 {
		t.Fatal("result duplicated")
	}
	call(t, c, "get_messages", map[string]any{"cursor": inbox["next_cursor"], "types": []string{"RESULT"}})
	if job := call(t, w, "claim_verification", map[string]any{})["job"]; job != nil {
		t.Fatal("worker obtained creator verification")
	}
	job := call(t, c, "claim_verification", map[string]any{})["job"].(map[string]any)
	verified := call(t, c, "verify_result", map[string]any{"task_id": id, "attempt_id": job["attempt_id"], "target_message_id": "mcp-result", "verdict": "verified"})
	if verified["task"].(map[string]any)["status"] != "COMPLETED" {
		t.Fatal("task not completed")
	}
	if err := credentials.RevokeCurrent(ctx, worker.Token); err != nil {
		t.Fatal(err)
	}
	out, err := w.CallTool(ctx, &mcp.CallToolParams{Name: "list_tasks", Arguments: map[string]any{}})
	if err != nil || !out.IsError {
		t.Fatalf("revoked credential accepted: %+v %v", out, err)
	}
}

func TestMCPRejectsPathInjectionAndDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; t.Error("redirect target reached") }))
	defer destination.Close()
	requests := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; http.Redirect(w, r, destination.URL, 302) }))
	defer h.Close()
	s, err := New(h.URL, "agent", "secret-fixture")
	if err != nil {
		t.Fatal(err)
	}
	client := connect(t, s)
	for _, id := range []string{"../credentials/revoke", "x?token=secret", "%2fadmin"} {
		out, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "claim_task", Arguments: map[string]any{"task_id": id}})
		if err != nil || !out.IsError {
			t.Fatalf("unsafe id: %s %v", id, err)
		}
	}
	if requests != 0 {
		t.Fatal("invalid IDs reached API")
	}
	out, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"task_id": "task-1"}})
	if err != nil || !out.IsError || hits != 0 {
		t.Fatalf("redirect handling: %+v %v", out, err)
	}
	if strings.Contains(fmt.Sprint(out.Content), "secret-fixture") {
		t.Fatal("credential in tool error")
	}
}

func TestMCPRejectsUnsafeConfiguration(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:password@example.com", "https://example.com?key=x", "https://example.com/other", "file:///tmp/server"} {
		if _, err := New(base, "agent", "token"); err == nil {
			t.Errorf("accepted %s", base)
		}
	}
}

func TestMCPForwardsArgumentsAndExplicitCursors(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, queryKey, queryValue string
		args                                     map[string]any
	}{
		{"list_tasks", "GET", "/tasks", "cursor", "page-2", map[string]any{"cursor": "page-2", "project_id": "private", "tools": []string{"shell"}}},
		{"get_messages", "GET", "/messages", "cursor", "message-page", map[string]any{"cursor": "message-page", "types": []string{"ANSWER"}}},
		{"search_memory", "GET", "/memory/search", "q", "a&b", map[string]any{"query": "a&b", "project_id": "private"}},
		{"get_artifact", "GET", "/artifacts/artifact-1", "", "", map[string]any{"artifact_id": "artifact-1"}},
		{"register_artifact", "POST", "/artifacts", "", "", map[string]any{"type": "log", "uri": "https://example.com/report.txt", "checksum": "sha256:fixture"}},
		{"request_operator", "POST", "/operator/requests", "", "", map[string]any{"kind": "wish", "title": "improvement", "details": "details"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("wrong forwarding: %s %s", r.Method, r.URL.Path)
				}
				if tc.queryKey != "" && r.URL.Query().Get(tc.queryKey) != tc.queryValue {
					t.Errorf("query not preserved: %s", r.URL.RawQuery)
				}
				if tc.method == "POST" {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					for key, value := range tc.args {
						if body[key] != value {
							t.Errorf("body field %s not preserved", key)
						}
					}
				}
				fmt.Fprint(w, `{"next_cursor":"next-page"}`)
			}))
			defer h.Close()
			s, err := New(h.URL, "agent", "fixture-key")
			if err != nil {
				t.Fatal(err)
			}
			if out := call(t, connect(t, s), tc.name, tc.args); out["next_cursor"] != "next-page" {
				t.Fatal("cursor dropped")
			}
		})
	}
}
