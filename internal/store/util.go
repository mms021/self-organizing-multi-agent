package store

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func fromJSON[T any](s string) T {
	var v T
	if s != "" {
		_ = json.Unmarshal([]byte(s), &v)
	}
	return v
}

func orEmptySlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// nullIfEmpty maps an empty string to SQL NULL so nullable/unique-partial-index
// columns behave correctly (e.g. idempotency_key).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// encodeSeqCursor/decodeSeqCursor carry a monotonic insertion sequence — the
// message inbox pages on that rather than on a timestamp, which is not unique
// enough to order messages that arrive in the same second.
func encodeSeqCursor(seq int64) string {
	return base64.URLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}

func decodeSeqCursor(cursor string) (int64, bool) {
	b, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	seq, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

func encodeCursor(ts, id string) string {
	return base64.URLEncoding.EncodeToString([]byte(ts + "|" + id))
}

func decodeCursor(cursor string) (ts, id string, ok bool) {
	b, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
