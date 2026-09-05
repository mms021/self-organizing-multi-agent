// Package db opens the SQLite database and applies the embedded schema.
package db

import (
	"database/sql"
	"fmt"

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
	return sqlDB, nil
}
