package store

import (
	"context"
	"testing"
	"time"

	"aichatdeck/internal/model"
)

func TestVerificationOperatorRetryAndAlerts(t *testing.T) {
	s, task, _ := submittedFixture(t)
	ctx := context.Background()
	if _, err := s.Tasks.RetryVerification(ctx, 1, task.TaskID); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("reset pending job: %v", err)
	}
	if _, err := s.Tasks.db.Exec(`UPDATE verification_jobs SET attempts=5,lease_until=? WHERE task_id=?`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), task.TaskID); err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.Tasks.ClaimVerificationAlert(ctx); err != nil || id != "" {
		t.Fatalf("live fifth attempt alerted: %s %v", id, err)
	}
	if _, err := s.Tasks.RetryVerification(ctx, 1, task.TaskID); !model.IsCode(err, model.ErrConflict) {
		t.Fatalf("reset live job: %v", err)
	}
	if _, err := s.Tasks.db.Exec(`UPDATE verification_jobs SET lease_until='' WHERE task_id=?`, task.TaskID); err != nil {
		t.Fatal(err)
	}
	id, token, err := s.Tasks.ClaimVerificationAlert(ctx)
	if err != nil || id != task.TaskID || token == "" {
		t.Fatalf("claim alert: %s %s %v", id, token, err)
	}
	if id, _, err := s.Tasks.ClaimVerificationAlert(ctx); err != nil || id != "" {
		t.Fatalf("duplicate alert lease: %s %v", id, err)
	}
	// Simulate a failed send/restart by expiring the durable sending lease.
	if _, err := s.Tasks.db.Exec(`UPDATE verification_alerts SET lease_until=''`); err != nil {
		t.Fatal(err)
	}
	id, newToken, err := s.Tasks.ClaimVerificationAlert(ctx)
	if err != nil || id != task.TaskID || newToken == token {
		t.Fatalf("alert retry: %s %v", id, err)
	}
	if err := s.Tasks.MarkVerificationAlertSent(ctx, id, newToken); err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.Tasks.ClaimVerificationAlert(ctx); err != nil || id != "" {
		t.Fatalf("sent alert repeated: %s %v", id, err)
	}
	if fresh, err := s.Tasks.RetryVerification(ctx, 100, task.TaskID); err != nil || !fresh {
		t.Fatalf("manual retry: %v %v", fresh, err)
	}
	job, err := s.Tasks.ClaimVerification(ctx, task.CreatedBy)
	if err != nil || job == nil || job.Attempts != 1 {
		t.Fatalf("new round: %+v %v", job, err)
	}
	if fresh, err := s.Tasks.RetryVerification(ctx, 100, task.TaskID); err != nil || fresh {
		t.Fatalf("duplicate update: %v %v", fresh, err)
	}
	if _, err := s.Tasks.db.Exec(`UPDATE verification_jobs SET attempts=5,lease_until=''`); err != nil {
		t.Fatal(err)
	}
	if fresh, err := s.Tasks.RetryVerification(ctx, 100, task.TaskID); err != nil || fresh {
		t.Fatalf("replay reset later round: %v %v", fresh, err)
	}
	// A late acknowledgement from the previous round must not hide this alert.
	if err := s.Tasks.MarkVerificationAlertSent(ctx, task.TaskID, newToken); err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.Tasks.ClaimVerificationAlert(ctx); err != nil || id != task.TaskID {
		t.Fatalf("new exhaustion not alerted: %s %v", id, err)
	}
	got, err := s.Tasks.Get(ctx, task.CreatedBy, task.TaskID)
	if err != nil || got.Status != model.TaskSubmitted {
		t.Fatalf("task execution restarted: %+v %v", got, err)
	}
}
