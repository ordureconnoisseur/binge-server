package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Additive column migrations applied after the base schema. SQLite has
// no "ADD COLUMN IF NOT EXISTS", so a duplicate-column error just means
// the migration already ran on this DB — tolerate it.
var addColumnMigrations = []string{
	// saved_at: stamped when a PornHub video is saved to Stash through
	// binge, so the feed/stories stop surfacing it as new.
	`ALTER TABLE pornhub_videos ADD COLUMN saved_at TEXT`,
}

func Open(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	// The config table holds the Stash API key and the Reddit/X session
	// cookies in plaintext, so keep the file owner-only. Best-effort:
	// a shared-host reader is the threat, and on Windows this is a no-op
	// that must not fail the open. Skip ":memory:" and DSN-only paths.
	if path != "" && path != ":memory:" {
		_ = os.Chmod(path, 0o600)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, stmt := range addColumnMigrations {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate add-column: %w", err)
		}
	}
	return db, nil
}
