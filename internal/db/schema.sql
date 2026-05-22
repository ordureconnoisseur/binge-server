CREATE TABLE IF NOT EXISTS performers (
  stash_id        INTEGER PRIMARY KEY,
  name            TEXT NOT NULL,
  image_path      TEXT NOT NULL DEFAULT '',
  favorite        INTEGER NOT NULL DEFAULT 0,
  reddit_handle   TEXT NOT NULL,
  handle_kind     TEXT NOT NULL,                    -- 'user' or 'sub'
  handle_status   TEXT NOT NULL DEFAULT 'ok',       -- 'ok' | 'suspended' | 'notfound' | 'unavailable'
  last_polled_at  TEXT,
  synced_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_performers_polled ON performers(last_polled_at);
CREATE INDEX IF NOT EXISTS idx_performers_status ON performers(handle_status);

CREATE TABLE IF NOT EXISTS posts (
  reddit_id          TEXT PRIMARY KEY,              -- 't3_<base36>'
  performer_stash_id INTEGER NOT NULL REFERENCES performers(stash_id) ON DELETE CASCADE,
  kind               TEXT NOT NULL,                 -- 'image' | 'video' | 'text' | 'link'
  title              TEXT,
  body               TEXT,
  media_url          TEXT,
  link_url           TEXT,
  thumb_url          TEXT,
  permalink          TEXT NOT NULL,
  domain             TEXT,
  is_nsfw            INTEGER NOT NULL DEFAULT 0,
  created_utc        INTEGER NOT NULL,
  fetched_at         TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_posts_performer ON posts(performer_stash_id, created_utc DESC);
CREATE INDEX IF NOT EXISTS idx_posts_created ON posts(created_utc DESC);

CREATE TABLE IF NOT EXISTS sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT
);

-- Live config — populated via /config POST from the binge UI (and
-- optionally seeded from env vars at startup for back-compat). Holds
-- the Reddit session cookie + Stash API key + Stash URL so the daemon
-- can be reconfigured without a restart.
CREATE TABLE IF NOT EXISTS config (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
