package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"aichatdeck/internal/model"
)

type recordingVerifier struct {
	EchoBrain
	calls int
	input VerificationInput
	task  model.Task
}

func (b *recordingVerifier) Verify(_ context.Context, task model.Task, input VerificationInput) (string, string, error) {
	b.calls++
	b.task, b.input = task, input
	return "verified", "reviewed", nil
}

func TestVerificationLoadsArtifactMetadata(t *testing.T) {
	for _, scenario := range []string{"valid", "unavailable", "wrong-task", "wrong-sender", "expired", "partial", "bad-content"} {
		t.Run(scenario, func(t *testing.T) {
			taskID := "task-1"
			task := model.Task{TaskID: taskID, CreatedBy: "creator", Status: model.TaskSubmitted, SubmittedMessageID: "result-1", SuccessCriteria: []string{"tests pass"}, Constraints: []string{"offline"}}
			artifact := model.Artifact{ArtifactID: "artifact-1", TaskID: &taskID, CreatedBy: "worker", URI: "https://example.com/file", Checksum: contentHash("tests pass")}
			switch scenario {
			case "wrong-task":
				other := "other"
				artifact.TaskID = &other
			case "wrong-sender":
				artifact.CreatedBy = "other"
			case "expired":
				expired := time.Now().Add(-time.Hour)
				artifact.ExpiresAt = &expired
			}
			status := "success"
			if scenario == "partial" {
				status = "partial"
			}
			payload, _ := json.Marshal(model.ResultPayload{Summary: "done", Status: status, Artifacts: []string{"artifact-1", "artifact-1"}, Metrics: map[string]any{"passed": 3}})
			var submitted model.VerifyRequest
			loads, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/messages":
					json.NewEncoder(w).Encode(map[string]any{"messages": []model.Envelope{}})
				case "/verification/claim":
					json.NewEncoder(w).Encode(map[string]any{"job": model.VerificationJob{AttemptID: "attempt-1", Task: task, Result: model.Envelope{MessageID: "result-1", Type: "RESULT", TaskID: taskID, Sender: "worker", Payload: payload}}})
				case "/tasks/task-1":
					json.NewEncoder(w).Encode(task)
				case "/artifacts/artifact-1":
					loads++
					if scenario == "unavailable" {
						http.Error(w, "unavailable", 503)
						return
					}
					json.NewEncoder(w).Encode(artifact)
				case "/tasks/task-1/verify":
					writes++
					if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
						t.Error(err)
					}
					json.NewEncoder(w).Encode(model.VerifyResponse{Task: task})
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			brain := &recordingVerifier{}
			a := Agent{Client: NewClient(srv.URL), Brain: brain}
			a.artifactHTTP = &http.Client{Transport: artifactRoundTrip(func(r *http.Request) (*http.Response, error) {
				body := "tests pass"
				if scenario == "bad-content" {
					body = "tampered"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			a.Client.AgentID = "creator"
			if acted, err := a.handleInbox(context.Background(), 0); err != nil || !acted {
				t.Fatalf("inbox: %v %v", acted, err)
			}
			if loads != 1 || writes != 1 {
				t.Fatalf("loads=%d writes=%d", loads, writes)
			}
			if submitted.AttemptID != "attempt-1" {
				t.Fatal("verification attempt token not sent")
			}
			if scenario == "valid" || scenario == "partial" {
				if len(brain.input.Contents) != 1 || brain.input.Contents[0].Text != "tests pass" {
					t.Fatalf("verified content missing: %+v", brain.input)
				}
				if brain.calls != 1 || !reflect.DeepEqual(brain.task.SuccessCriteria, task.SuccessCriteria) || !reflect.DeepEqual(brain.task.Constraints, task.Constraints) || len(brain.input.Artifacts) != 1 || brain.input.Artifacts[0].URI != artifact.URI || brain.input.Result.Metrics["passed"] != float64(3) {
					t.Fatalf("incomplete verification input: %+v", brain)
				}
				if !reflect.DeepEqual(submitted.Evidence, []string{"artifact-1"}) {
					t.Fatalf("evidence not recorded: %+v", submitted)
				}
			} else if brain.calls != 0 || len(submitted.Evidence) != 0 {
				t.Fatal("unavailable evidence used for verification")
			}
			want := "inconclusive"
			if scenario == "valid" {
				want = "verified"
			}
			if submitted.Verdict != want {
				t.Fatalf("verdict=%s want=%s", submitted.Verdict, want)
			}
		})
	}
}

func TestVerificationPromptIncludesCriteriaAndMetadata(t *testing.T) {
	prompt := verifyPrompt(model.Task{Objective: "work", SuccessCriteria: []string{"all tests pass"}, Constraints: []string{"no network"}}, VerificationInput{Result: model.ResultPayload{Summary: "done", Status: "partial"}, Artifacts: []model.Artifact{{URI: attack, Checksum: "unverified-hash"}}, Contents: []ArtifactContent{{ArtifactID: "artifact-1", Text: attack, SHA256: "checked-hash"}}})
	for _, value := range []string{"all tests pass", "no network", "partial", "unverified-hash", "checked-hash", "artifact_content", "целостность, но не достоверность", "INCONCLUSIVE"} {
		if !strings.Contains(prompt, value) {
			t.Errorf("missing %q", value)
		}
	}
	nonce := promptNonce(t, prompt)
	if strings.Count(prompt, "[DATA "+nonce+" ") != strings.Count(prompt, "[/DATA "+nonce+"]") {
		t.Fatal("unbalanced data fences")
	}
}

func TestEchoDoesNotPretendToVerifyCriteria(t *testing.T) {
	verdict, _, err := (EchoBrain{}).Verify(context.Background(), model.Task{Objective: "work", SuccessCriteria: []string{"tests pass"}}, VerificationInput{Result: model.ResultPayload{Summary: "work", Status: "success"}})
	if err != nil || verdict != "inconclusive" {
		t.Fatalf("verdict=%s err=%v", verdict, err)
	}
}
