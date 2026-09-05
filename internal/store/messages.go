package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"aichatdeck/internal/model"
)

type MessageStore struct{ db *sql.DB }

func NewMessageStore(db *sql.DB) *MessageStore { return &MessageStore{db: db} }

const messageColumns = `message_id, protocol_version, type, sender, recipient, ts, conversation_id, task_id, priority, payload, evidence, reply_to, ttl, received_at`

// Insert persists env, deduplicating on message_id (RFC-1100 §8): a replay
// with the same message_id is a no-op that returns the originally-stored
// row, with isNew=false so the caller (the HTTP handler) knows not to
// re-publish a bus wakeup or re-run any other message side effect.
func (s *MessageStore) Insert(ctx context.Context, env model.Envelope) (model.Envelope, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	var replyTo, taskID, conversationID any
	if env.ReplyTo != nil {
		replyTo = *env.ReplyTo
	}
	if env.TaskID != "" {
		taskID = env.TaskID
	}
	if env.ConversationID != "" {
		conversationID = env.ConversationID
	}
	var ttl any
	if env.TTL != nil {
		ttl = *env.TTL
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO messages (`+messageColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		env.MessageID, env.ProtocolVersion, env.Type, env.Sender, env.Recipient,
		env.Timestamp.UTC().Format(time.RFC3339), conversationID, taskID, env.Priority,
		string(env.Payload), toJSON(orEmptySlice(env.Evidence)), replyTo, ttl, now,
	)
	if err != nil {
		return model.Envelope{}, false, err
	}
	n, _ := res.RowsAffected()

	stored, err := s.Get(ctx, env.MessageID)
	if err != nil {
		return model.Envelope{}, false, err
	}
	return stored, n > 0, nil
}

// scanMessage returns the envelope plus its server-assigned received_at
// (RFC-1100 never defines received_at — it's bookkeeping for List's keyset
// pagination, kept separate from the envelope's client-supplied timestamp).
func scanMessage(row interface{ Scan(...any) error }) (model.Envelope, string, error) {
	var e model.Envelope
	var tsStr, receivedAtStr, payloadStr, evidenceJSON string
	var conversationID, taskID, replyTo sql.NullString
	var ttl sql.NullInt64
	err := row.Scan(&e.MessageID, &e.ProtocolVersion, &e.Type, &e.Sender, &e.Recipient, &tsStr,
		&conversationID, &taskID, &e.Priority, &payloadStr, &evidenceJSON, &replyTo, &ttl, &receivedAtStr)
	if err != nil {
		return model.Envelope{}, "", err
	}
	if ts, perr := time.Parse(time.RFC3339, tsStr); perr == nil {
		e.Timestamp = ts
	}
	if conversationID.Valid {
		e.ConversationID = conversationID.String
	}
	if taskID.Valid {
		e.TaskID = taskID.String
	}
	if replyTo.Valid {
		e.ReplyTo = &replyTo.String
	}
	if ttl.Valid {
		v := int(ttl.Int64)
		e.TTL = &v
	}
	e.Payload = []byte(payloadStr)
	e.Evidence = fromJSON[[]string](evidenceJSON)
	return e, receivedAtStr, nil
}

func (s *MessageStore) Get(ctx context.Context, messageID string) (model.Envelope, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE message_id=?`, messageID)
	e, _, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Envelope{}, model.NotFound("message not found")
		}
		return model.Envelope{}, err
	}
	return e, nil
}

// MessageFilter is the GET /messages query.
type MessageFilter struct {
	Recipient string // the authenticated agent — always required
	TaskID    string
	Cursor    string
	Limit     int
}

// List returns messages addressed directly to filter.Recipient or to
// "broadcast", newest-received-last, keyset-paginated on (received_at,
// message_id), with expired (RFC-1100 §10) rows filtered out in Go.
func (s *MessageStore) List(ctx context.Context, filter MessageFilter, now time.Time) ([]model.Envelope, string, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	where := []string{"recipient IN (?, ?)"}
	args := []any{filter.Recipient, model.BroadcastRecipient}
	if filter.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, filter.TaskID)
	}
	if filter.Cursor != "" {
		ts, id, ok := decodeCursor(filter.Cursor)
		if !ok {
			return nil, "", model.ValidationError("invalid cursor")
		}
		where = append(where, "(received_at > ? OR (received_at = ? AND message_id > ?))")
		args = append(args, ts, ts, id)
	}

	q := `SELECT ` + messageColumns + ` FROM messages WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY received_at, message_id LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var msgs []model.Envelope
	var receivedAts []string
	for rows.Next() {
		e, receivedAt, err := scanMessage(rows)
		if err != nil {
			return nil, "", err
		}
		if e.Expired(now) {
			continue
		}
		msgs = append(msgs, e)
		receivedAts = append(receivedAts, receivedAt)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(msgs) > limit {
		nextCursor = encodeCursor(receivedAts[limit-1], msgs[limit-1].MessageID)
		msgs = msgs[:limit]
	}
	return msgs, nextCursor, nil
}
