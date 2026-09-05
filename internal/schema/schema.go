// Package schema embeds the SQLite DDL. No migration framework yet
// (5 tables, first milestone) — schema.sql is just re-run at startup with
// CREATE TABLE IF NOT EXISTS statements.
package schema

import _ "embed"

//go:embed schema.sql
var SQL string
