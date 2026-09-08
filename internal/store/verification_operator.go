package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

// Only expired/exhausted jobs qualify, never a live fifth attempt.
const exhaustedVerification = `j.done=0 AND j.attempts>=5 AND j.lease_until<=?
 AND t.status='SUBMITTED' AND t.submitted_message_id=j.message_id`

func (s *TaskStore) ExhaustedVerifications(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.task_id FROM verification_jobs j JOIN tasks t ON t.task_id=j.task_id WHERE `+exhaustedVerification+` ORDER BY j.task_id LIMIT 10`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RetryVerification is an operator-only operation. The Telegram update ledger
// and reset commit together, so a replay cannot reset a later round of work.
func (s *TaskStore) RetryVerification(ctx context.Context, updateID int64, taskID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM verification_retries WHERE update_id=?`, updateID).Scan(&existing)
	if err == nil {
		if existing != taskID {
			return false, model.Conflict("update already used")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `UPDATE verification_jobs SET attempts=0,next_at=?,attempt_id='',lease_until=''
 WHERE task_id IN (SELECT j.task_id FROM verification_jobs j JOIN tasks t ON t.task_id=j.task_id WHERE `+exhaustedVerification+` AND j.task_id=?)`, now, now, taskID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, model.Conflict("task has no exhausted verification; active and completed tasks cannot be reset")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO verification_retries(update_id,task_id,created_at) VALUES(?,?,?)`, updateID, taskID, now); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE verification_alerts SET sent=0,lease_until='',token='' WHERE task_id=?`, taskID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *TaskStore) ClaimVerificationAlert(ctx context.Context) (taskID, token string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO verification_alerts(task_id) SELECT j.task_id FROM verification_jobs j JOIN tasks t ON t.task_id=j.task_id WHERE `+exhaustedVerification, stamp)
	if err != nil {
		return "", "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT a.task_id FROM verification_alerts a JOIN verification_jobs j ON j.task_id=a.task_id JOIN tasks t ON t.task_id=j.task_id WHERE a.sent=0 AND a.lease_until<=? AND `+exhaustedVerification+` ORDER BY a.task_id LIMIT 1`, stamp, stamp).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	token = idgen.New("alert")
	_, err = tx.ExecContext(ctx, `UPDATE verification_alerts SET token=?,lease_until=? WHERE task_id=?`, token, now.Add(2*time.Minute).Format(time.RFC3339), taskID)
	if err != nil {
		return "", "", err
	}
	return taskID, token, tx.Commit()
}

func (s *TaskStore) MarkVerificationAlertSent(ctx context.Context, taskID, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE verification_alerts SET sent=1,token='',lease_until='' WHERE task_id=? AND token=?`, taskID, token)
	return err
}
