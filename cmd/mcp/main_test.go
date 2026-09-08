package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"aichatdeck/internal/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioHelper(t *testing.T) {
	if os.Getenv("AICHATDECK_TEST_STDIO") != "1" {
		return
	}
	os.Args = []string{"mcp", "-state", os.Getenv("AICHATDECK_TEST_STATE")}
	main()
	os.Exit(0)
}

func TestStdioMainInitializationAndToolCall(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stdio-fixture-key" {
			t.Error("wrong credential")
		}
		if r.URL.Path != "/tasks" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tasks":[],"next_cursor":""}`)
	}))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := agent.SaveState(path, agent.State{AgentID: "agent", Token: "stdio-fixture-key", BaseURL: api.URL}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdioHelper$")
	command.Env = append(os.Environ(), "AICHATDECK_TEST_STDIO=1", "AICHATDECK_TEST_STATE="+path)
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 13 {
		t.Fatalf("tools: %+v %v", listed, err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_tasks", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("stdio call: %+v %v", result, err)
	}
}
