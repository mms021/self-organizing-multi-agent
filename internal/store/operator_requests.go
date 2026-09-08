package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

type OperatorRequestStore struct{ db *sql.DB }

func NewOperatorRequestStore(db *sql.DB) *OperatorRequestStore { return &OperatorRequestStore{db: db} }

// DeliverReply commits the inbox message and request status together. The
// persisted update ID makes repeated reply deliveries harmless after restart.
func (s *OperatorRequestStore) DeliverReply(ctx context.Context, updateID, telegramMessageID int64, reply string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	messageID := idgen.New("message")
	var recipient string
	err = tx.QueryRowContext(ctx, "SELECT agent_id FROM telegram_updates WHERE update_id=?", updateID).Scan(&recipient)
	if err == nil {
		return recipient, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	record, err := scanOperatorRequest(tx.QueryRowContext(ctx, "SELECT "+operatorRequestColumns+" FROM operator_requests WHERE telegram_message_id=?", telegramMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, model.NotFound("operator request not found")
	}
	if err != nil {
		return "", false, err
	}
	payload, err := json.Marshal(model.AnswerPayload{Answer: reply, References: []string{record.RequestID}})
	if err != nil {
		return "", false, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `INSERT INTO messages (message_id,protocol_version,type,sender,recipient,ts,priority,payload,reply_to,received_at)
 VALUES (?,'1.0','ANSWER','platform',?,?,'normal',?,?,?)`, messageID, record.AgentID, now, string(payload), record.RequestID, now)
	if err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE operator_requests SET status='answered',reply=?,answered_at=? WHERE request_id=?", reply, now, record.RequestID); err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO telegram_updates(update_id,agent_id,message_id) VALUES (?,?,?)", updateID, record.AgentID, messageID); err != nil {
		return "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return record.AgentID, true, nil
}

const operatorRequestColumns = `request_id, agent_id, kind, title, details, status, telegram_message_id, reply, created_at, answered_at`

func scanOperatorRequest(row interface{ Scan(...any) error }) (model.OperatorRequestRecord, error) {
	var record model.OperatorRequestRecord
	var messageID sql.NullInt64
	var reply, createdAt, answeredAt sql.NullString
	err := row.Scan(&record.RequestID, &record.AgentID, &record.Kind, &record.Title, &record.Details, &record.Status, &messageID, &reply, &createdAt, &answeredAt)
	if err != nil {
		return model.OperatorRequestRecord{}, err
	}
	if messageID.Valid {
		value := messageID.Int64
		record.TelegramMessageID = &value
	}
	if reply.Valid {
		record.Reply = reply.String
	}
	if value, err := time.Parse(time.RFC3339, createdAt.String); err == nil {
		record.CreatedAt = value
	}
	if answeredAt.Valid {
		if value, err := time.Parse(time.RFC3339, answeredAt.String); err == nil {
			record.AnsweredAt = &value
		}
	}
	return record, nil
}

func (s *OperatorRequestStore) Create(ctx context.Context, agentID string, req model.OperatorRequest) (model.OperatorRequestRecord, error) {
	id, now := idgen.New("operator_request"), time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `INSERT INTO operator_requests (request_id, agent_id, kind, title, details, status, created_at) VALUES (?,?,?,?,?,'pending',?)`, id, agentID, req.Kind, req.Title, req.Details, now)
	if err != nil {
		return model.OperatorRequestRecord{}, err
	}
	return s.Get(ctx, id)
}

func (s *OperatorRequestStore) Get(ctx context.Context, id string) (model.OperatorRequestRecord, error) {
	record, err := scanOperatorRequest(s.db.QueryRowContext(ctx, `SELECT `+operatorRequestColumns+` FROM operator_requests WHERE request_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.OperatorRequestRecord{}, model.NotFound("operator request not found")
	}
	return record, err
}

func (s *OperatorRequestStore) SetTelegramMessage(ctx context.Context, id string, messageID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operator_requests SET status='sent', telegram_message_id=? WHERE request_id=?`, messageID, id)
	return err
}

func (s *OperatorRequestStore) SetFailed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operator_requests SET status='failed' WHERE request_id=?`, id)
	return err
}

func (s *OperatorRequestStore) ByTelegramMessage(ctx context.Context, messageID int64) (model.OperatorRequestRecord, error) {
	record, err := scanOperatorRequest(s.db.QueryRowContext(ctx, `SELECT `+operatorRequestColumns+` FROM operator_requests WHERE telegram_message_id=?`, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.OperatorRequestRecord{}, model.NotFound("operator request not found")
	}
	return record, err
}

func (s *OperatorRequestStore) Answer(ctx context.Context, id, reply string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operator_requests SET status='answered', reply=?, answered_at=? WHERE request_id=?`, reply, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *OperatorRequestStore) List(ctx context.Context, limit int) ([]model.OperatorRequestRecord, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+operatorRequestColumns+` FROM operator_requests ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []model.OperatorRequestRecord
	for rows.Next() {
		record, err := scanOperatorRequest(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
