// Package db opens the SQLite database and applies the embedded schema.
package db

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"aichatdeck/internal/schema"
)

// Open opens (creating if needed) the SQLite file at path, sets pragmas
// sensible for a single-process server, and applies schema.SQL.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite is not safe for concurrent writers on one *sql.DB
	// beyond what SQLite itself serializes; a single connection avoids
	// SQLITE_BUSY races under WAL for this milestone's traffic level.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema.SQL); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureTaskLeaseColumns(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate task leases: %w", err)
	}
	if _, err := sqlDB.Exec(`ALTER TABLE agents ADD COLUMN tools TEXT NOT NULL DEFAULT '[]'`); err != nil && !containsColumn(err) {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate agent tools: %w", err)
	}
	// Backfill accepted results from older databases without resetting jobs on restart.
	if _, err := sqlDB.Exec(`INSERT OR IGNORE INTO verification_jobs(task_id, message_id, next_at)
		SELECT task_id, submitted_message_id, created_at FROM tasks
		WHERE status='SUBMITTED' AND submitted_message_id IS NOT NULL`); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate verification queue: %w", err)
	}
	return sqlDB, nil
}

// ensureTaskLeaseColumns upgrades databases created before task leases were
// introduced. SQLite's CREATE TABLE IF NOT EXISTS cannot add columns itself.
func ensureTaskLeaseColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range []string{"claimed_at", "lease_expires_at", "claim_id", "submitted_message_id"} {
		if !have[column] {
			if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN ` + column + ` TEXT`); err != nil {
				return err
			}
		}
	}
	if !have["required_tools"] {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN required_tools TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return err
		}
	}
	return nil
}

func containsColumn(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "duplicate column") || strings.Contains(err.Error(), "already exists"))
}
