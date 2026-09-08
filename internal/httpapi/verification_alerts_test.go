package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aichatdeck/internal/db"
	"aichatdeck/internal/model"
	"aichatdeck/internal/store"
)

type alertMessenger struct {
	messages []string
	fail     bool
}

func (m *alertMessenger) Notify(context.Context, string, model.OperatorRequest) (int64, error) {
	return 1, nil
}
func (m *alertMessenger) Send(_ context.Context, text string) (int64, error) {
	if m.fail {
		return 0, errors.New("test failure")
	}
	m.messages = append(m.messages, text)
	return int64(len(m.messages)), nil
}

func TestVerificationAlertsAndAuthorizedRetry(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	agents := store.NewAgentStore(database)
	creator, err := agents.Create(ctx, model.RegisterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := agents.Create(ctx, model.RegisterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tasks := store.NewTaskStore(database)
	task, err := tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "alert test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := tasks.Claim(ctx, task.TaskID, worker.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(model.ResultPayload{Summary: "done", Status: "success", ClaimID: claim.ClaimID})
	if _, _, err := store.NewMessageStore(database).Insert(ctx, model.Envelope{MessageID: "result", Type: "RESULT", Sender: worker.AgentID, Recipient: creator.AgentID, TaskID: task.TaskID, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE verification_jobs SET attempts=5,lease_until=''`); err != nil {
		t.Fatal(err)
	}
	messenger := &alertMessenger{fail: true}
	s := &Server{DB: database, Tasks: tasks, OperatorRequests: store.NewOperatorRequestStore(database), OperatorNotifier: messenger, TelegramUserID: "42", TelegramWebhookSecret: "test-secret"}
	if err := s.sendVerificationAlerts(ctx); err == nil {
		t.Fatal("failed notification acknowledged")
	}
	if _, err := database.Exec(`UPDATE verification_alerts SET lease_until=''`); err != nil {
		t.Fatal(err)
	}
	messenger.fail = false
	if err := s.sendVerificationAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.sendVerificationAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.messages) != 1 || !strings.Contains(messenger.messages[0], "/retry "+task.TaskID) {
		t.Fatalf("alerts: %v", messenger.messages)
	}
	webhook := func(user int, secret string) int {
		r := httptest.NewRequest("POST", "/telegram/webhook", strings.NewReader(fmt.Sprintf(`{"update_id":123,"message":{"message_id":5,"from":{"id":%d},"text":"/retry %s"}}`, user, task.TaskID)))
		r.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		w := httptest.NewRecorder()
		s.handleTelegramWebhook(w, r)
		return w.Code
	}
	if code := webhook(42, "wrong"); code != 404 {
		t.Fatalf("wrong secret: %d", code)
	}
	if code := webhook(99, "test-secret"); code != 200 {
		t.Fatalf("wrong user: %d", code)
	}
	var attempts int
	if err := database.QueryRow(`SELECT attempts FROM verification_jobs`).Scan(&attempts); err != nil || attempts != 5 {
		t.Fatalf("unauthorized reset: %d %v", attempts, err)
	}
	if code := webhook(42, "test-secret"); code != 200 {
		t.Fatalf("retry: %d", code)
	}
	if code := webhook(42, "test-secret"); code != 200 {
		t.Fatalf("replay: %d", code)
	}
	if err := database.QueryRow(`SELECT attempts FROM verification_jobs`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("retry did not reset: %d %v", attempts, err)
	}
	if len(messenger.messages) != 2 {
		t.Fatalf("duplicate command acknowledgement: %v", messenger.messages)
	}
	s.Credentials = store.NewCredentialStore(database)
	key, err := s.Credentials.Issue(ctx, creator.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []int{99, 42, 42} {
		r := httptest.NewRequest("POST", "/telegram/webhook", strings.NewReader(fmt.Sprintf(`{"update_id":124,"message":{"from":{"id":%d},"text":"/revoke %s"}}`, user, creator.AgentID)))
		r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "test-secret")
		w := httptest.NewRecorder()
		s.handleTelegramWebhook(w, r)
		if w.Code != 200 {
			t.Fatalf("operator revoke: %d", w.Code)
		}
		_, _, active, err := s.Credentials.LookupAgentByToken(ctx, key.Token)
		if err != nil || active != (user == 99) {
			t.Fatalf("revocation authorization user=%d active=%v err=%v", user, active, err)
		}
	}
}
