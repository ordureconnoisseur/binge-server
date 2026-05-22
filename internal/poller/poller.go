package poller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/reddit"
	"github.com/ordureconnoisseur/binge-server/internal/redgifs"
	"github.com/ordureconnoisseur/binge-server/internal/stash"
)

// Page size for listing fetches. Reddit's max is 100; 25 keeps per-call
// payloads small and is more than enough between polls.
const listingLimit = 25

// Between-request pacing so we don't burst Reddit. 600 req / 10 min
// is the budget; ~100ms is plenty of headroom.
const perRequestSleep = 100 * time.Millisecond

// Retention window — posts older than this get swept nightly.
const retentionDays = 90

type Poller struct {
	db      *sql.DB
	store   *configstore.Store
	stash   *stash.Client
	reddit  *reddit.Client
	redgifs *redgifs.Client
	log     *slog.Logger

	performerSyncInterval time.Duration
	pollInterval          time.Duration

	// Rate-limit the "waiting for config" log line so a permanently
	// unconfigured daemon doesn't fill logs with one entry per tick.
	warnMu        sync.Mutex
	lastWarnedAt  time.Time
}

func New(
	db *sql.DB,
	store *configstore.Store,
	stashClient *stash.Client,
	redditClient *reddit.Client,
	redgifsClient *redgifs.Client,
	log *slog.Logger,
	performerSyncInterval, pollInterval time.Duration,
) *Poller {
	return &Poller{
		db:                    db,
		store:                 store,
		stash:                 stashClient,
		reddit:                redditClient,
		redgifs:               redgifsClient,
		log:                   log,
		performerSyncInterval: performerSyncInterval,
		pollInterval:          pollInterval,
	}
}

// applyConfig pushes the current config-store values into the Reddit
// and Stash clients. Returns false (with a rate-limited warning) if a
// required credential is missing — the calling tick handler short-
// circuits in that case.
func (p *Poller) applyConfig() bool {
	stashURL := p.store.Get(configstore.KeyStashURL)
	apiKey := p.store.Get(configstore.KeyStashAPIKey)
	cookie := p.store.Get(configstore.KeyRedditCookie)
	if stashURL == "" || apiKey == "" || cookie == "" {
		p.warnMu.Lock()
		if time.Since(p.lastWarnedAt) > time.Hour {
			p.log.Warn("waiting for config — POST /config from the binge UI",
				"stash_url_set", stashURL != "",
				"stash_api_key_set", apiKey != "",
				"reddit_cookie_set", cookie != "")
			p.lastWarnedAt = time.Now()
		}
		p.warnMu.Unlock()
		return false
	}
	p.stash.SetCredentials(stashURL, apiKey)
	p.reddit.SetCookie(cookie)
	return true
}

// Run starts the two background loops + retention sweep. Returns when
// ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	// Initial pass: only proceed if config is present. A freshly-
	// installed daemon with no env vars will skip both passes and rely
	// on the first /config POST to wake things up via /reddit/refresh.
	if p.applyConfig() {
		if err := p.SyncPerformers(ctx); err != nil {
			p.log.Error("initial performer sync failed", "err", err)
		}
		if err := p.PollAll(ctx); err != nil {
			p.log.Error("initial poll failed", "err", err)
		}
	}

	performerTick := time.NewTicker(p.performerSyncInterval)
	defer performerTick.Stop()
	pollTick := time.NewTicker(p.pollInterval)
	defer pollTick.Stop()
	retentionTick := time.NewTicker(24 * time.Hour)
	defer retentionTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-performerTick.C:
			if !p.applyConfig() {
				continue
			}
			if err := p.SyncPerformers(ctx); err != nil {
				p.log.Error("performer sync failed", "err", err)
			}
		case <-pollTick.C:
			if !p.applyConfig() {
				continue
			}
			if err := p.PollAll(ctx); err != nil {
				p.log.Error("poll failed", "err", err)
			}
		case <-retentionTick.C:
			if err := p.sweepOldPosts(ctx); err != nil {
				p.log.Error("retention sweep failed", "err", err)
			}
		}
	}
}

// ── Performer sync ────────────────────────────────────────────────────

var (
	reRedditUser = regexp.MustCompile(`(?i)reddit\.com/(?:user|u)/([^/?#\s]+)`)
	reRedditSub  = regexp.MustCompile(`(?i)reddit\.com/r/([^/?#\s]+)`)
)

// parseRedditHandle returns (handle, kind, ok). Prefers `user` over
// `sub` when both are present, per plan.
func parseRedditHandle(urls []string) (string, string, bool) {
	var subHandle string
	for _, u := range urls {
		if m := reRedditUser.FindStringSubmatch(u); len(m) == 2 {
			return strings.TrimSuffix(m[1], "/"), "user", true
		}
		if subHandle == "" {
			if m := reRedditSub.FindStringSubmatch(u); len(m) == 2 {
				subHandle = strings.TrimSuffix(m[1], "/")
			}
		}
	}
	if subHandle != "" {
		return subHandle, "sub", true
	}
	return "", "", false
}

// SyncPerformers fetches all Stash performers and upserts the ones
// with reddit URLs. Performers without (or who lost) reddit URLs are
// deleted — posts cascade via FK.
func (p *Poller) SyncPerformers(ctx context.Context) error {
	start := time.Now()
	const pageSize = 200
	keepStashIDs := make(map[int]bool, 1024)
	total := 0
	for page := 1; ; page++ {
		performers, count, err := p.stash.FetchPerformersPage(ctx, page, pageSize)
		if err != nil {
			return err
		}
		if err := p.upsertPerformersBatch(ctx, performers, keepStashIDs); err != nil {
			return err
		}
		total += len(performers)
		if page*pageSize >= count || len(performers) == 0 {
			break
		}
		if page > 200 {
			return fmt.Errorf("performer sync exceeded 200 pages — bad pagination?")
		}
	}

	// Delete performers no longer present in Stash OR no longer linked
	// to a reddit handle. Cascade removes their posts.
	if err := p.deleteMissingPerformers(ctx, keepStashIDs); err != nil {
		return err
	}

	_, _ = p.db.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('last_performer_sync', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().UTC().Format(time.RFC3339))
	p.log.Info("performer sync done", "scanned", total, "with_reddit", len(keepStashIDs), "elapsed", time.Since(start))
	return nil
}

func (p *Poller) upsertPerformersBatch(ctx context.Context, performers []stash.Performer, keep map[int]bool) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, perf := range performers {
		handle, kind, ok := parseRedditHandle(perf.URLs)
		if !ok {
			continue
		}
		id, _ := strconv.Atoi(perf.ID)
		fav := 0
		if perf.Favorite {
			fav = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO performers(stash_id, name, image_path, favorite, reddit_handle, handle_kind, handle_status, synced_at)
			VALUES(?,?,?,?,?,?, COALESCE((SELECT handle_status FROM performers WHERE stash_id=?), 'ok'), datetime('now'))
			ON CONFLICT(stash_id) DO UPDATE SET
				name=excluded.name,
				image_path=excluded.image_path,
				favorite=excluded.favorite,
				reddit_handle=excluded.reddit_handle,
				handle_kind=excluded.handle_kind,
				synced_at=datetime('now')`,
			id, perf.Name, perf.ImagePath, fav, handle, kind, id)
		if err != nil {
			return fmt.Errorf("upsert performer %d: %w", id, err)
		}
		keep[id] = true
	}
	return tx.Commit()
}

func (p *Poller) deleteMissingPerformers(ctx context.Context, keep map[int]bool) error {
	rows, err := p.db.QueryContext(ctx, `SELECT stash_id FROM performers`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var toDelete []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !keep[id] {
			toDelete = append(toDelete, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(toDelete) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range toDelete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM performers WHERE stash_id=?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.log.Info("removed performers no longer linked to reddit", "count", len(toDelete))
	return nil
}

// ── Reddit polling ────────────────────────────────────────────────────

type pollTarget struct {
	StashID    int
	Handle     string
	HandleKind string
}

// PollAll polls every active performer in turn. On rate-limit it
// bails out and waits for the next tick.
func (p *Poller) PollAll(ctx context.Context) error {
	start := time.Now()
	rows, err := p.db.QueryContext(ctx, `
		SELECT stash_id, reddit_handle, handle_kind
		FROM performers
		WHERE handle_status='ok' AND reddit_handle != ''
		ORDER BY COALESCE(last_polled_at, '') ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var targets []pollTarget
	for rows.Next() {
		var t pollTarget
		if err := rows.Scan(&t.StashID, &t.Handle, &t.HandleKind); err != nil {
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	inserted := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := p.pollOne(ctx, t)
		inserted += n
		if err != nil {
			if errors.Is(err, reddit.ErrRateLimit) {
				p.log.Warn("rate-limited by reddit — bailing out until next tick")
				break
			}
			if errors.Is(err, reddit.ErrCookieExpired) {
				p.log.Error("Reddit session cookie invalid or expired — refresh it via the binge settings page")
				break
			}
			p.log.Warn("poll performer failed", "stash_id", t.StashID, "handle", t.Handle, "err", err)
			continue
		}
		time.Sleep(perRequestSleep)
	}

	_, _ = p.db.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('last_poll', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().UTC().Format(time.RFC3339))
	p.log.Info("poll cycle done", "performers", len(targets), "new_posts", inserted, "elapsed", time.Since(start))
	return nil
}

func (p *Poller) pollOne(ctx context.Context, t pollTarget) (int, error) {
	var (
		posts []reddit.Post
		err   error
	)
	switch t.HandleKind {
	case "user":
		posts, err = p.reddit.FetchUserSubmissions(ctx, t.Handle, listingLimit)
	case "sub":
		posts, err = p.reddit.FetchSubNew(ctx, t.Handle, listingLimit)
	default:
		return 0, fmt.Errorf("unknown handle_kind %q", t.HandleKind)
	}
	if err != nil {
		// Mark suspended/notfound/forbidden so we stop polling.
		switch {
		case errors.Is(err, reddit.ErrNotFound):
			p.markStatus(ctx, t.StashID, "notfound")
		case errors.Is(err, reddit.ErrSuspended):
			p.markStatus(ctx, t.StashID, "suspended")
		case errors.Is(err, reddit.ErrForbidden):
			p.markStatus(ctx, t.StashID, "unavailable")
		}
		return 0, err
	}

	inserted := 0
	for _, post := range posts {
		c := reddit.Classify(post)
		mediaURL := c.MediaURL
		if c.NeedsRedgifs && c.RedgifsSlug != "" {
			r, rerr := p.redgifs.Resolve(ctx, c.RedgifsSlug)
			if rerr != nil {
				p.log.Warn("redgifs resolve failed",
					"stash_id", t.StashID, "reddit_id", post.ID, "slug", c.RedgifsSlug, "err", rerr)
				// Fall back to the original reddit url as link — still
				// shows up in the feed, opens in a new tab to redgifs.
				c.Kind = "link"
				c.LinkURL = post.URL
			} else if r.HD != "" {
				mediaURL = r.HD
			} else if r.SD != "" {
				mediaURL = r.SD
			} else if r.GIF != "" {
				mediaURL = r.GIF
			}
		}

		nsfw := 0
		if post.Over18 {
			nsfw = 1
		}
		res, ierr := p.db.ExecContext(ctx, `
			INSERT INTO posts(reddit_id, performer_stash_id, kind, title, body, media_url, link_url, thumb_url, permalink, domain, is_nsfw, created_utc, fetched_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,datetime('now'))
			ON CONFLICT(reddit_id) DO NOTHING`,
			post.Name,
			t.StashID,
			c.Kind,
			nullable(post.Title),
			nullable(post.Selftext),
			nullable(mediaURL),
			nullable(c.LinkURL),
			nullable(c.ThumbURL),
			reddit.PermalinkURL(post),
			nullable(c.Domain),
			nsfw,
			int64(post.CreatedUTC),
		)
		if ierr != nil {
			p.log.Warn("insert post failed",
				"stash_id", t.StashID, "reddit_id", post.Name, "err", ierr)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}

	_, _ = p.db.ExecContext(ctx, `
		UPDATE performers SET last_polled_at=datetime('now') WHERE stash_id=?`, t.StashID)
	return inserted, nil
}

func (p *Poller) markStatus(ctx context.Context, stashID int, status string) {
	_, err := p.db.ExecContext(ctx, `
		UPDATE performers SET handle_status=?, last_polled_at=datetime('now') WHERE stash_id=?`,
		status, stashID)
	if err != nil {
		p.log.Warn("markStatus failed", "stash_id", stashID, "status", status, "err", err)
	}
}

// ── Retention ─────────────────────────────────────────────────────────

func (p *Poller) sweepOldPosts(ctx context.Context) error {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM posts WHERE created_utc < ?`, cutoff)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		p.log.Info("retention sweep", "deleted", n, "older_than_days", retentionDays)
	}
	return nil
}

// nullable returns a value suitable for sql.DB.Exec — empty strings
// become SQL NULLs so queries don't have to coalesce.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
