package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

const maxVerificationAttempts = 5
const verificationLease = 5 * time.Minute

func verificationBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > maxVerificationAttempts {
		attempts = maxVerificationAttempts
	}
	return time.Minute * time.Duration(1<<(attempts-1))
}

func (s *TaskStore) ClaimVerification(ctx context.Context, viewer string) (*model.VerificationJob, error) {
	return s.claimVerificationAt(ctx, viewer, time.Now().UTC())
}

func (s *TaskStore) claimVerificationAt(ctx context.Context, viewer string, now time.Time) (*model.VerificationJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339)
	var taskID, messageID string
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT j.task_id, j.message_id, j.attempts FROM verification_jobs j
		JOIN tasks t ON t.task_id=j.task_id
		WHERE j.done=0 AND j.attempts<? AND j.next_at<=? AND j.lease_until<=?
		AND t.status='SUBMITTED' AND t.submitted_message_id=j.message_id AND t.created_by=? AND t.owner<>?
		AND (t.project_id IS NULL OR EXISTS (SELECT 1 FROM projects p WHERE p.project_id=t.project_id AND p.status='active'
		AND (p.visibility='open' OR EXISTS (SELECT 1 FROM project_members m WHERE m.project_id=p.project_id AND m.agent_id=? AND m.status='active'))))
		ORDER BY j.next_at,j.task_id LIMIT 1`, maxVerificationAttempts, stamp, stamp, viewer, viewer, viewer).Scan(&taskID, &messageID, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job := &model.VerificationJob{AttemptID: idgen.New("verify-attempt"), Attempts: attempts + 1, LeaseUntil: now.Add(verificationLease)}
	_, err = tx.ExecContext(ctx, `UPDATE verification_jobs SET attempts=attempts+1, attempt_id=?, lease_until=?, next_at=? WHERE task_id=?`,
		job.AttemptID, job.LeaseUntil.UTC().Format(time.RFC3339), job.LeaseUntil.Add(verificationBackoff(job.Attempts)).UTC().Format(time.RFC3339), taskID)
	if err != nil {
		return nil, err
	}
	job.Task, err = scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=?`, taskID))
	if err != nil {
		return nil, err
	}
	job.Result, _, err = scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageSelectColumns+` FROM messages WHERE message_id=?`, messageID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

// Finish in the same transaction as the verdict. A late process cannot commit
// a verdict for a different attempt, even when both processes share an agent ID.
func finishVerificationJob(ctx context.Context, tx *sql.Tx, taskID string, req model.VerifyRequest, now time.Time) error {
	var attemptID, lease string
	var attempts int
	err := tx.QueryRowContext(ctx, `SELECT attempt_id,lease_until,attempts FROM verification_jobs WHERE task_id=?`, taskID).Scan(&attemptID, &lease, &attempts)
	if errors.Is(err, sql.ErrNoRows) && req.AttemptID == "" {
		return nil
	} // legacy/manual record
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(time.RFC3339)
	if req.AttemptID != "" {
		if attemptID != req.AttemptID || lease <= stamp {
			return model.Conflict("verification attempt is stale or expired")
		}
	} else if attemptID != "" && lease > stamp {
		return model.Conflict("verification is leased; provide its attempt_id or wait for expiry")
	}
	done := req.Verdict != "inconclusive"
	_, err = tx.ExecContext(ctx, `UPDATE verification_jobs SET done=?, attempt_id='', lease_until='', next_at=? WHERE task_id=?`, done, now.Add(verificationBackoff(attempts)).UTC().Format(time.RFC3339), taskID)
	return err
}
