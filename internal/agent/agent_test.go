package agent_test

import (
	"context"
	"io"
	"log"
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
)

func newPlatform(t *testing.T) *httptest.Server {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "agent-e2e.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

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
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAgent(t *testing.T, base, name string) *agent.Agent {
	t.Helper()
	a := &agent.Agent{
		Client: agent.NewClient(base),
		Brain:  agent.EchoBrain{},
		Name:   name,
		Caps:   []string{"testing"},
		Log:    log.New(io.Discard, "", 0),
	}
	if err := a.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap %s: %v", name, err)
	}
	return a
}

// TestTwoAgentsCompleteATask is the point of the whole client: no human in
// the loop. One agent posts work, the other finds it, does it, and the first
// verifies the result — driven only by each agent's own iteration.
func TestTwoAgentsCompleteATask(t *testing.T) {
	ctx := context.Background()
	platform := newPlatform(t)

	creator := newAgent(t, platform.URL, "creator")
	worker := newAgent(t, platform.URL, "worker")

	task, err := creator.Client.CreateTask(ctx, model.CreateTaskRequest{Objective: "count to three"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Worker: finds the open task, claims it, works it, reports a RESULT.
	acted, err := worker.RunOnce(ctx, 0)
	if err != nil {
		t.Fatalf("worker iteration: %v", err)
	}
	if !acted {
		t.Fatal("worker did nothing; expected it to claim the open task")
	}

	claimed, err := creator.Client.GetTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if claimed.Status != model.TaskClaimed {
		t.Fatalf("expected CLAIMED after worker iteration, got %s", claimed.Status)
	}

	// Creator: sees the RESULT in its inbox and verifies it.
	if _, err := creator.RunOnce(ctx, 0); err != nil {
		t.Fatalf("creator iteration: %v", err)
	}

	final, err := creator.Client.GetTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if final.Status != model.TaskCompleted {
		t.Fatalf("expected COMPLETED after creator verified, got %s", final.Status)
	}
}

// Knowledge must actually flow: an agent publishes what it learned, and the
// next agent working a related task finds it and works with it in hand.
func TestKnowledgeIsPublishedAndReused(t *testing.T) {
	ctx := context.Background()
	platform := newPlatform(t)

	creator := newAgent(t, platform.URL, "creator")
	worker := newAgent(t, platform.URL, "worker")

	first, err := creator.Client.CreateTask(ctx, model.CreateTaskRequest{Objective: "index the archive"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := worker.RunOnce(ctx, 0); err != nil {
		t.Fatalf("worker iteration: %v", err)
	}

	// Working the task must have left a lesson behind.
	entries, err := creator.Client.SearchKnowledge(ctx, "index the archive", nil, 10)
	if err != nil {
		t.Fatalf("search knowledge: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no knowledge published after completing a task")
	}
	lesson := entries[0]
	if lesson.Category != model.KnowledgeLesson {
		t.Errorf("expected a lesson, got %s", lesson.Category)
	}
	if lesson.Status != model.KnowledgeProposed {
		t.Errorf("a self-reported lesson must start as proposed, got %s", lesson.Status)
	}
	if lesson.TaskID == nil || *lesson.TaskID != first.TaskID {
		t.Errorf("lesson is not linked back to its task: %+v", lesson.TaskID)
	}

	// A second, similar task should now be worked with that lesson in hand —
	// EchoBrain reports how many entries it was handed.
	if _, err := creator.Client.CreateTask(ctx, model.CreateTaskRequest{Objective: "index the archive"}); err != nil {
		t.Fatalf("create second task: %v", err)
	}
	if _, err := worker.RunOnce(ctx, 0); err != nil {
		t.Fatalf("second worker iteration: %v", err)
	}

	msgs, err := creator.Client.Inbox(ctx, 0)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	for _, m := range msgs {
		if m.Type == "RESULT" && strings.Contains(string(m.Payload), "reused") {
			return
		}
	}
	t.Fatal("second task was worked without reusing the published knowledge")
}

// The creator must not claim its own task: it would then be the RESULT's
// sender and the platform would refuse to let it verify (self-verification).
func TestAgentSkipsItsOwnTask(t *testing.T) {
	ctx := context.Background()
	platform := newPlatform(t)
	creator := newAgent(t, platform.URL, "creator")

	if _, err := creator.Client.CreateTask(ctx, model.CreateTaskRequest{Objective: "own work"}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	acted, err := creator.RunOnce(ctx, 0)
	if err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if acted {
		t.Fatal("agent claimed its own task; it would be unable to verify the result")
	}
}

// Bootstrap must announce itself with the ZZ marker from prompt.md, so the
// message log shows whether an agent is running with the instructions loaded.
func TestBootstrapAnnouncesZZMarker(t *testing.T) {
	ctx := context.Background()
	platform := newPlatform(t)

	watcher := newAgent(t, platform.URL, "watcher") // subscribed before the next agent starts
	newAgent(t, platform.URL, "loud")

	msgs, err := watcher.Client.Inbox(ctx, 0)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	for _, m := range msgs {
		if m.Type == "STATUS" && strings.Contains(string(m.Payload), "ZZ") {
			return
		}
	}
	t.Fatalf("no broadcast STATUS carrying the ZZ marker; got %d messages", len(msgs))
}

// prompt.md is embedded into the binary and handed to the model as its system
// prompt — if it ever ships empty, agents run with no instructions at all.
func TestSystemPromptEmbedded(t *testing.T) {
	if !strings.HasPrefix(agent.SystemPrompt, "ZZ") {
		t.Fatalf("system prompt must start with the ZZ marker, got %.20q", agent.SystemPrompt)
	}
	if !strings.Contains(agent.SystemPrompt, "POST /tasks/{task_id}/claim") {
		t.Error("system prompt is missing the claim endpoint")
	}
}
