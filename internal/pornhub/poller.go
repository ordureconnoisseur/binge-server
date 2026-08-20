package pornhub

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/stash"
)

// Recent videos pulled per performer per poll. Bounds the one-time
// backfill; steady-state only the new ones get a metadata fetch.
const listLimit = 24

// Pace between performers / metadata fetches so the one-time backfill
// (121 performers x ~20 videos) doesn't get the single Mullvad exit IP
// rate-limited/blocked by PornHub. Steady-state only new videos fetch, so
// being generous here costs little.
const perPerformerSleep = 3 * time.Second
const perMetaSleep = 2 * time.Second

type Poller struct {
	db    *sql.DB
	store *configstore.Store
	stash *stash.Client
	ph    *Client
	log   *slog.Logger

	syncInterval, pollInterval time.Duration
}

func NewPoller(db *sql.DB, store *configstore.Store, st *stash.Client, ph *Client, log *slog.Logger, syncInterval, pollInterval time.Duration) *Poller {
	return &Poller{db: db, store: store, stash: st, ph: ph, log: log, syncInterval: syncInterval, pollInterval: pollInterval}
}

func (p *Poller) ready() bool {
	if p.store.Get(configstore.KeyStashURL) == "" || p.store.Get(configstore.KeyStashAPIKey) == "" {
		return false
	}
	p.stash.SetCredentials(p.store.Get(configstore.KeyStashURL), p.store.Get(configstore.KeyStashAPIKey))
	return true
}

func (p *Poller) Run(ctx context.Context) {
	if p.ready() {
		if err := p.SyncPerformers(ctx); err != nil {
			p.log.Error("initial pornhub performer sync failed", "err", err)
		}
		go func() {
			if err := p.PollAll(ctx); err != nil {
				p.log.Error("initial pornhub poll failed", "err", err)
			}
		}()
	}
	syncTick := time.NewTicker(p.syncInterval)
	defer syncTick.Stop()
	pollTick := time.NewTicker(p.pollInterval)
	defer pollTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncTick.C:
			if p.ready() {
				if err := p.SyncPerformers(ctx); err != nil {
					p.log.Error("pornhub performer sync failed", "err", err)
				}
			}
		case <-pollTick.C:
			if p.ready() {
				if err := p.PollAll(ctx); err != nil {
					p.log.Error("pornhub poll failed", "err", err)
				}
			}
		}
	}
}

// SyncPerformers upserts Stash performers that have a pornhub URL and
// deletes those that don't (cascading their videos).
func (p *Poller) SyncPerformers(ctx context.Context) error {
	start := time.Now()
	const pageSize = 200
	keep := make(map[int]bool, 256)
	total := 0
	reported := 0
	for page := 1; ; page++ {
		performers, count, err := p.stash.FetchPerformersPage(ctx, page, pageSize)
		if err != nil {
			return err
		}
		reported = count
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, perf := range performers {
			url := ModelURLFromURLs(perf.URLs)
			if url == "" {
				continue
			}
			id, convErr := strconv.Atoi(perf.ID)
			if convErr != nil || id == 0 {
				p.log.Warn("skipping performer with a non-numeric stash id",
					"id", perf.ID, "name", perf.Name)
				continue
			}
			fav := 0
			if perf.Favorite {
				fav = 1
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO pornhub_performers(stash_id, name, image_path, favorite, ph_url, ph_status, synced_at)
				VALUES(?,?,?,?,?, COALESCE((SELECT ph_status FROM pornhub_performers WHERE stash_id=?),'ok'), datetime('now'))
				ON CONFLICT(stash_id) DO UPDATE SET
					name=excluded.name, image_path=excluded.image_path,
					favorite=excluded.favorite, ph_url=excluded.ph_url, synced_at=datetime('now')`,
				id, perf.Name, perf.ImagePath, fav, url, id)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("upsert pornhub performer %d: %w", id, err)
			}
			keep[id] = true
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		total += len(performers)
		if page*pageSize >= count || len(performers) == 0 {
			break
		}
		if page > 200 {
			return fmt.Errorf("pornhub performer sync exceeded 200 pages")
		}
	}
	// Same rule as the reddit poller: a short read is not a licence to
	// delete what was not seen.
	if reported > 0 && total < reported {
		return fmt.Errorf(
			"stash reported %d performers but only %d arrived; not reconciling against a partial list",
			reported, total)
	}
	if err := p.deleteMissing(ctx, keep); err != nil {
		return err
	}
	p.log.Info("pornhub performer sync done", "scanned", total, "with_pornhub", len(keep), "elapsed", time.Since(start))
	return nil
}

// pruneSignature identifies a proposed removal so an unusually large
// one can be recognised when offered again.
func pruneSignature(ids []int) string {
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

func (p *Poller) deleteMissing(ctx context.Context, keep map[int]bool) error {
	rows, err := p.db.QueryContext(ctx, `SELECT stash_id FROM pornhub_performers`)
	if err != nil {
		return err
	}
	var del []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !keep[id] {
			del = append(del, id)
		}
	}
	rows.Close()
	if len(del) == 0 {
		// A clean sync forgets any removal remembered from before.
		_, _ = p.db.ExecContext(ctx,
			`DELETE FROM sync_state WHERE key='pending_prune_pornhub'`)
		return nil
	}
	// Confirmed by repetition, exactly as the reddit poller does it,
	// and with its own remembered signature.
	//
	// A previous version guarded only the empty answer here, reasoning
	// that a large removal would be caught by the reddit side because
	// both tables are reconciled from the same Stash. That was wrong on
	// two counts: the two tables are built from different subsets of
	// that Stash, so one can lose most of its rows while the other
	// loses none, and this deletion commits before the reddit poller's
	// refusal happens at all. Three of four performers, and the videos
	// cached against them, went in a single sync while the reddit side
	// was busy refusing.
	var existing int
	_ = p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pornhub_performers`).Scan(&existing)
	sig := pruneSignature(del)
	var lastSig string
	_ = p.db.QueryRowContext(ctx,
		`SELECT value FROM sync_state WHERE key='pending_prune_pornhub'`).Scan(&lastSig)
	if existing > 0 {
		refuse := len(keep) == 0
		if !refuse && len(del)*2 >= existing && sig != lastSig {
			refuse = true
		}
		if refuse {
			_, _ = p.db.ExecContext(ctx,
				`INSERT INTO sync_state(key,value) VALUES('pending_prune_pornhub', ?)
				 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, sig)
			p.log.Error("refusing to prune pornhub performers for now",
				"linked_in_stash", len(keep), "in_db", existing, "would_remove", len(del))
			return fmt.Errorf("refusing to prune pornhub performers")
		}
	}
	_, _ = p.db.ExecContext(ctx,
		`DELETE FROM sync_state WHERE key='pending_prune_pornhub'`)
	for _, id := range del {
		if _, err := p.db.ExecContext(ctx, `DELETE FROM pornhub_performers WHERE stash_id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// PollAll lists each performer's recent videos and fetches metadata for
// the ones not already cached.
func (p *Poller) PollAll(ctx context.Context) error {
	start := time.Now()
	rows, err := p.db.QueryContext(ctx, `
		SELECT stash_id, ph_url FROM pornhub_performers
		WHERE ph_status='ok' ORDER BY COALESCE(last_polled_at,'') ASC`)
	if err != nil {
		return err
	}
	type target struct {
		id  int
		url string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.url); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	rows.Close()

	inserted := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := p.pollOne(ctx, t.id, t.url)
		inserted += n
		if err != nil {
			p.log.Warn("pornhub poll performer failed", "stash_id", t.id, "err", err)
		}
		_, _ = p.db.ExecContext(ctx, `UPDATE pornhub_performers SET last_polled_at=datetime('now') WHERE stash_id=?`, t.id)
		time.Sleep(perPerformerSleep)
	}
	_, _ = p.db.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('last_pornhub_poll', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().UTC().Format(time.RFC3339))
	p.log.Info("pornhub poll cycle done", "performers", len(targets), "new_videos", inserted, "elapsed", time.Since(start))
	return nil
}

func (p *Poller) pollOne(ctx context.Context, stashID int, modelURL string) (int, error) {
	refs, err := p.ph.ListVideos(ctx, modelURL, listLimit)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, r := range refs {
		// Already cached? skip the metadata fetch.
		var exists int
		_ = p.db.QueryRowContext(ctx, `SELECT 1 FROM pornhub_videos WHERE video_id=?`, r.ViewKey).Scan(&exists)
		if exists == 1 {
			continue
		}
		meta, err := p.ph.FetchVideoMeta(ctx, r.URL)
		if err != nil {
			p.log.Warn("pornhub meta fetch failed", "stash_id", stashID, "viewkey", r.ViewKey, "err", err)
			time.Sleep(perMetaSleep)
			continue
		}
		title := meta.Title
		if title == "" {
			title = r.Title
		}
		res, ierr := p.db.ExecContext(ctx, `
			INSERT INTO pornhub_videos(video_id, performer_stash_id, title, url, thumb_url, duration, view_count, upload_date, created_utc, fetched_at)
			VALUES(?,?,?,?,?,?,?,?,?,datetime('now'))
			ON CONFLICT(video_id) DO NOTHING`,
			meta.ViewKey, stashID, nullable(title), r.URL, nullable(meta.ThumbURL),
			meta.Duration, meta.ViewCount, nullable(meta.UploadDate), meta.CreatedUtc)
		if ierr != nil {
			p.log.Warn("pornhub video insert failed", "viewkey", r.ViewKey, "err", ierr)
		} else if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
		time.Sleep(perMetaSleep)
	}
	return inserted, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
