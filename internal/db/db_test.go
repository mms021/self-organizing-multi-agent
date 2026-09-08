package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"aichatdeck/internal/schema"
)

func TestOpenMigratesAttemptColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(schema.SQL, ",\n  claim_id TEXT,\n  submitted_message_id TEXT", "", 1)
	if oldSchema == schema.SQL {
		t.Fatal("legacy schema fixture did not remove columns")
	}
	if _, err := legacy.Exec(oldSchema); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		migrated, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := migrated.Query(`SELECT claim_id, submitted_message_id FROM tasks`)
		if err != nil {
			migrated.Close()
			t.Fatal(err)
		}
		rows.Close()
		migrated.Close()
	}
}
