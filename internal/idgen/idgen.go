// Package idgen generates IDs and tokens without pulling in an external uuid
// dependency — crypto/rand is all a v4 UUID or a random token needs.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// UUIDv4 returns a random RFC-4122 v4 UUID string.
func UUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// New returns a "prefix-uuid" identifier, e.g. "agent-<uuid>".
func New(prefix string) string {
	return prefix + "-" + UUIDv4()
}

// Token returns a random 32-byte credential token, hex-encoded.
func Token() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
