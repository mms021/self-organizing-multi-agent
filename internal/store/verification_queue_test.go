package store

import (
	"context"
	"testing"
	"time"

	"aichatdeck/internal/db"
	"aichatdeck/internal/model"
)

func submittedFixture(t *testing.T) (*AgentStoreSet, model.Task, model.Envelope) {
	t.Helper()
	s := openTestDB(t)
	ctx := context.Background()
	creator, worker := mustCreateAgent(t, s.Agents), mustCreateAgent(t, s.Agents)
	task, err := s.Tasks.Create(ctx, creator.AgentID, model.CreateTaskRequest{Objective: "queue test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Tasks.Claim(ctx, task.TaskID, worker.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	msg := model.Envelope{MessageID: "queued-result", Type: "RESULT", Sender: worker.AgentID, Recipient: creator.AgentID, TaskID: task.TaskID, Payload: []byte(toJSON(model.ResultPayload{Summary: "done", Status: "success", ClaimID: claim.ClaimID}))}
	if _, _, err := s.Msgs.Insert(ctx, msg); err != nil {
		t.Fatal(err)
	}
	return s, task, msg
}

func TestVerificationQueueFencingAndRetry(t *testing.T) {
	ctx := context.Background()
	s, task, msg := submittedFixture(t)
	other := mustCreateAgent(t, s.Agents)
	if job, err := s.Tasks.ClaimVerification(ctx, other.AgentID); err != nil || job != nil {
		t.Fatalf("other creator got job: %+v %v", job, err)
	}
	first, err := s.Tasks.ClaimVerification(ctx, task.CreatedBy)
	if err != nil || first == nil {
		t.Fatalf("claim: %+v %v", first, err)
	}
	if first.Attempts != 1 || first.Result.MessageID != msg.MessageID || first.Task.Status != model.TaskSubmitted {
		t.Fatalf("bad job: %+v", first)
	}
	if job, err := s.Tasks.ClaimVerification(ctx, task.CreatedBy); err != nil || job != nil {
		t.Fatalf("duplicate claim: %+v %v", job, err)
	}
	req := model.VerifyRequest{TargetMessageID: msg.MessageID, Verdict: "verified", AttemptID: "wrong"}
	for _, token := range []string{"wrong", ""} {
		req.AttemptID = token
		if _, _, err := s.Tasks.Verify(ctx, task.TaskID, task.CreatedBy, req); !model.IsCode(err, model.ErrConflict) {
			t.Fatalf("unfenced verification: %v", err)
		}
	}
	// A failed verdict write must roll back the job completion as well.
	if _, err := s.Tasks.db.Exec(`CREATE TRIGGER fail_verdict BEFORE INSERT ON verifications BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	req.AttemptID = first.AttemptID
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, task.CreatedBy, req); err == nil {
		t.Fatal("expected injected failure")
	}
	var token string
	if err := s.Tasks.db.QueryRow(`SELECT attempt_id FROM verification_jobs WHERE task_id=?`, task.TaskID).Scan(&token); err != nil || token != first.AttemptID {
		t.Fatalf("queue mutation not rolled back: %s %v", token, err)
	}
	if _, err := s.Tasks.db.Exec(`DROP TRIGGER fail_verdict`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.db.Exec(`UPDATE verification_jobs SET lease_until='',next_at='' WHERE task_id=?`, task.TaskID); err != nil {
		t.Fatal(err)
	}
	second, err := s.Tasks.ClaimVerification(ctx, task.CreatedBy)
	if err != nil || second == nil || second.AttemptID == first.AttemptID {
		t.Fatalf("recovery: %+v %v", second, err)
	}
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, task.CreatedBy, req); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("old process committed: %v", err)
	}
	req.AttemptID, req.Verdict = second.AttemptID, "inconclusive"
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, task.CreatedBy, req); err != nil {
		t.Fatal(err)
	}
	if job, err := s.Tasks.ClaimVerification(ctx, task.CreatedBy); err != nil || job != nil {
		t.Fatalf("retry without backoff: %+v %v", job, err)
	}
	var next string
	if err := s.Tasks.db.QueryRow(`SELECT next_at FROM verification_jobs WHERE task_id=?`, task.TaskID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse(time.RFC3339, next)
	if err != nil || time.Until(due) < 110*time.Second {
		t.Fatalf("second backoff: %s %v", next, err)
	}
	third, err := s.Tasks.claimVerificationAt(ctx, task.CreatedBy, due)
	if err != nil || third == nil || third.Attempts != 3 {
		t.Fatalf("retry: %+v %v", third, err)
	}
	req.AttemptID, req.Verdict = third.AttemptID, "verified"
	if _, _, err := s.Tasks.Verify(ctx, task.TaskID, task.CreatedBy, req); err != nil {
		t.Fatal(err)
	}
	if job, err := s.Tasks.claimVerificationAt(ctx, task.CreatedBy, due.Add(time.Hour)); err != nil || job != nil {
		t.Fatalf("completed job retried: %+v %v", job, err)
	}
}

func TestVerificationQueueCrashAttemptLimit(t *testing.T) {
	s, task, _ := submittedFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 1; i <= maxVerificationAttempts; i++ {
		job, err := s.Tasks.claimVerificationAt(ctx, task.CreatedBy, now)
		if err != nil || job == nil || job.Attempts != i {
			t.Fatalf("attempt %d: %+v %v", i, job, err)
		}
		now = job.LeaseUntil.Add(verificationBackoff(i))
	}
	if job, err := s.Tasks.claimVerificationAt(ctx, task.CreatedBy, now.Add(time.Hour)); err != nil || job != nil {
		t.Fatalf("retry limit ignored: %+v %v", job, err)
	}
	got, err := s.Tasks.Get(ctx, task.CreatedBy, task.TaskID)
	if err != nil || got.Status != model.TaskSubmitted {
		t.Fatalf("exhaustion changed task: %+v %v", got, err)
	}
}

func TestVerificationQueueSurvivesRestartAndBackfills(t *testing.T) {
	s, task, _ := submittedFixture(t)
	ctx := context.Background()
	var path string
	if err := s.Tasks.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-queue accepted task, then reopen the actual database file.
	if _, err := s.Tasks.db.Exec(`DELETE FROM verification_jobs`); err != nil {
		t.Fatal(err)
	}
	if err := s.Tasks.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	jobs := NewTaskStore(reopened)
	first, err := jobs.ClaimVerification(ctx, task.CreatedBy)
	if err != nil || first == nil {
		reopened.Close()
		t.Fatalf("backfill: %+v %v", first, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	jobs = NewTaskStore(reopened)
	if job, err := jobs.ClaimVerification(ctx, task.CreatedBy); err != nil || job != nil {
		t.Fatalf("restart reset live lease: %+v %v", job, err)
	}
	second, err := jobs.claimVerificationAt(ctx, task.CreatedBy, first.LeaseUntil.Add(verificationBackoff(1)))
	if err != nil || second == nil || second.Attempts != 2 {
		t.Fatalf("restart recovery: %+v %v", second, err)
	}
}

func TestVerificationQueueHonorsProjectAccess(t *testing.T) {
	for _, state := range []string{"revoked", "reviewer", "archived", "active"} {
		t.Run(state, func(t *testing.T) {
			s, task, _ := submittedFixture(t)
			ctx := context.Background()
			project, err := NewProjectStore(s.Tasks.db).Create(ctx, task.CreatedBy, model.CreateProjectRequest{Name: "private", Visibility: "closed"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Tasks.db.Exec(`UPDATE tasks SET project_id=? WHERE task_id=?`, project.ProjectID, task.TaskID); err != nil {
				t.Fatal(err)
			}
			if state == "archived" {
				if _, err := s.Tasks.db.Exec(`UPDATE projects SET status='archived' WHERE project_id=?`, project.ProjectID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.Tasks.db.Exec(`UPDATE project_members SET status=? WHERE project_id=? AND agent_id=?`, state, project.ProjectID, task.CreatedBy); err != nil {
					t.Fatal(err)
				}
			}
			job, err := s.Tasks.ClaimVerification(ctx, task.CreatedBy)
			if err != nil {
				t.Fatal(err)
			}
			if (job != nil) != (state == "active") {
				t.Fatalf("state=%s job=%+v", state, job)
			}
		})
	}
}

func TestSubmissionAndQueueAreAtomic(t *testing.T) {
	s, original, msg := submittedFixture(t)
	ctx := context.Background()
	task, err := s.Tasks.Create(ctx, original.CreatedBy, model.CreateTaskRequest{Objective: "atomic queue"}, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Tasks.Claim(ctx, task.TaskID, msg.Sender)
	if err != nil {
		t.Fatal(err)
	}
	msg.MessageID, msg.TaskID = "atomic-result", task.TaskID
	msg.Payload = []byte(toJSON(model.ResultPayload{Summary: "done", Status: "success", ClaimID: claim.ClaimID}))
	if _, err := s.Tasks.db.Exec(`CREATE TRIGGER fail_enqueue BEFORE INSERT ON verification_jobs BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Msgs.Insert(ctx, msg); err == nil {
		t.Fatal("expected enqueue failure")
	}
	if _, err := s.Msgs.Get(ctx, msg.MessageID); !model.IsCode(err, model.ErrNotFound) {
		t.Fatalf("message escaped rollback: %v", err)
	}
	got, err := s.Tasks.Get(ctx, original.CreatedBy, task.TaskID)
	if err != nil || got.Status != model.TaskClaimed {
		t.Fatalf("submission escaped rollback: %+v %v", got, err)
	}
	if _, err := s.Tasks.db.Exec(`DROP TRIGGER fail_enqueue`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Msgs.Insert(ctx, msg); err != nil {
		t.Fatal(err)
	}
}
