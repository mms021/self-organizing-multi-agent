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

// seq is assigned by SQLite, so it appears in reads but never in writes.
const (
	messageInsertColumns = `message_id, protocol_version, type, sender, recipient, ts, conversation_id, task_id, priority, payload, evidence, reply_to, ttl, received_at`
	messageSelectColumns = `seq, ` + messageInsertColumns
)

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
		INSERT OR IGNORE INTO messages (`+messageInsertColumns+`)
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

// scanMessage returns the envelope plus its server-assigned seq — the
// monotonic insertion sequence List paginates on. Neither seq nor received_at
// is part of the RFC-1100 envelope; they are server-side bookkeeping, kept
// separate from the envelope's client-supplied timestamp.
func scanMessage(row interface{ Scan(...any) error }) (model.Envelope, int64, error) {
	var e model.Envelope
	var seq int64
	var tsStr, receivedAtStr, payloadStr, evidenceJSON string
	var conversationID, taskID, replyTo sql.NullString
	var ttl sql.NullInt64
	err := row.Scan(&seq, &e.MessageID, &e.ProtocolVersion, &e.Type, &e.Sender, &e.Recipient, &tsStr,
		&conversationID, &taskID, &e.Priority, &payloadStr, &evidenceJSON, &replyTo, &ttl, &receivedAtStr)
	if err != nil {
		return model.Envelope{}, 0, err
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
	_ = receivedAtStr // stored for audit/debugging; ordering uses seq
	return e, seq, nil
}

func (s *MessageStore) Get(ctx context.Context, messageID string) (model.Envelope, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+messageSelectColumns+` FROM messages WHERE message_id=?`, messageID)
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
	// Types narrows delivery to the message types the caller actually acts
	// on. Without it every agent drains every broadcast just to discard it,
	// which is the noise RFC-1100 §15 asks agents to avoid.
	Types  []string
	Cursor string
	Limit  int
}

// List returns messages addressed directly to filter.Recipient or to
// "broadcast", oldest first, keyset-paginated on the insertion sequence
// (seq), with expired (RFC-1100 §10) rows filtered out in Go.
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
	if len(filter.Types) > 0 {
		where = append(where, "type IN ("+strings.TrimSuffix(strings.Repeat("?,", len(filter.Types)), ",")+")")
		for _, t := range filter.Types {
			args = append(args, t)
		}
	}
	if filter.Cursor != "" {
		seq, ok := decodeSeqCursor(filter.Cursor)
		if !ok {
			return nil, "", model.ValidationError("invalid cursor")
		}
		where = append(where, "seq > ?")
		args = append(args, seq)
	}

	q := `SELECT ` + messageSelectColumns + ` FROM messages WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY seq LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var msgs []model.Envelope
	var seqs []int64
	for rows.Next() {
		e, seq, err := scanMessage(rows)
		if err != nil {
			return nil, "", err
		}
		// An expired row is skipped but still advances the cursor past it —
		// otherwise the caller would re-scan it on every poll forever.
		seqs = append(seqs, seq)
		if e.Expired(now) {
			continue
		}
		msgs = append(msgs, e)
		if len(msgs) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	// The cursor is a resume token, not a has-more flag: it always points past
	// the last row examined, so a polling inbox never re-reads a message it has
	// already seen. An empty page keeps the caller's existing position.
	nextCursor := filter.Cursor
	if n := len(seqs); n > 0 {
		nextCursor = encodeSeqCursor(seqs[n-1])
	}
	return msgs, nextCursor, nil
}
