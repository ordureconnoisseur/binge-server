// Package configstore is a thin SQLite-backed key/value store for the
// daemon's runtime configuration (Stash URL/API key, Reddit session
// cookie). Reads are O(1) from an in-memory cache; writes hit both
// disk and the cache under a write lock.
//
// Keys are stable strings (see KeyStashURL, KeyStashAPIKey,
// KeyRedditCookie). Empty string means "not set" — the poller skips
// its cycle when required creds are empty.
package configstore

import (
	"database/sql"
	"sync"
)

const (
	KeyStashURL      = "stash_url"
	KeyStashAPIKey   = "stash_api_key"
	KeyRedditCookie  = "reddit_session_cookie"
)

type Store struct {
	db    *sql.DB
	mu    sync.RWMutex
	cache map[string]string
}

// New hydrates the cache from the existing rows in the config table.
// Subsequent reads never touch the DB.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db, cache: make(map[string]string)}
	rows, err := db.Query(`SELECT key, value FROM config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		s.cache[k] = v
	}
	return s, rows.Err()
}

// Get returns the stored value for key, or empty string if unset.
func (s *Store) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache[key]
}

// Set persists the value to the DB + cache. Empty values are kept as
// empty strings (we never delete rows — simpler semantics).
func (s *Store) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO config(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	if err != nil {
		return err
	}
	s.cache[key] = value
	return nil
}

// SetIfEmpty seeds a value only if the key is currently unset. Used to
// migrate env-var startup config into the store on first boot without
// clobbering values the user has set via the UI later.
func (s *Store) SetIfEmpty(key, value string) error {
	if value == "" {
		return nil
	}
	s.mu.RLock()
	already := s.cache[key]
	s.mu.RUnlock()
	if already != "" {
		return nil
	}
	return s.Set(key, value)
}
