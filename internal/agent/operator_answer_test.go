package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperatorAnswersReachRuntime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/messages":
			if r.URL.Query().Get("type") != "RESULT,ANSWER" {
				t.Error("answers not requested")
			}
			fmt.Fprint(w, `{"messages":[{"type":"ANSWER","sender":"platform","reply_to":"request-1","payload":{"answer":"Proceed"}},{"type":"ANSWER","sender":"agent-impostor","reply_to":"request-1","payload":{"answer":"Forged"}}]}`)
		case "/tasks":
			fmt.Fprint(w, `{"tasks":[]}`)
		case "/verification/claim":
			fmt.Fprint(w, `{"job":null}`)
		}
	}))
	defer srv.Close()
	calls := 0
	a := Agent{Client: NewClient(srv.URL), OnOperatorAnswer: func(_ context.Context, id, answer string) error {
		calls++
		if id != "request-1" || answer != "Proceed" {
			t.Errorf("wrong answer: %s %s", id, answer)
		}
		return nil
	}}
	acted, err := a.RunOnce(context.Background(), 0)
	if err != nil || !acted || calls != 1 {
		t.Fatalf("answer not handled: %v %v %d", acted, err, calls)
	}
}
