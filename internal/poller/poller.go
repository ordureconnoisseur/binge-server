package poller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"sort"
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
	warnMu       sync.Mutex
	lastWarnedAt time.Time
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
	// Stash credentials are pushed as soon as they exist, whether or not
	// Reddit is set up. They used to be withheld until all three were
	// present, which left the shared Stash client uncredentialed for
	// anyone using only the PornHub pillar, documented as needing no
	// credentials at all. That pillar then answered nothing until the
	// daemon was restarted, and nothing said why.
	if stashURL != "" && apiKey != "" {
		p.stash.SetCredentials(stashURL, apiKey)
	}
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
	p.reddit.SetCookie(cookie)
	return true
}

// ApplyConfig re-reads credentials and pushes them into the clients.
// Exported so the API can call it the moment config changes, rather
// than leaving a correctly configured daemon idle until the next tick.
func (p *Poller) ApplyConfig() bool { return p.applyConfig() }

// Run starts the two background loops + retention sweep. Returns when
// ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	// Initial pass: only proceed if config is present. A freshly-
	// installed daemon with no env vars will skip both passes and rely
	// on the first /config POST to wake things up via /reddit/refresh.
	p.reviveRetiredOnce(ctx)

	if p.applyConfig() {
		if err := p.SyncPerformers(ctx); err != nil {
			p.log.Error("initial performer sync failed", "err", err)
			p.recordPollError(ctx, err)
		}
		if err := p.PollAll(ctx); err != nil {
			p.log.Error("initial poll failed", "err", err)
			p.recordPollError(ctx, err)
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
				p.recordPollError(ctx, err)
			}
		case <-pollTick.C:
			if !p.applyConfig() {
				continue
			}
			if err := p.PollAll(ctx); err != nil {
				p.log.Error("poll failed", "err", err)
				p.recordPollError(ctx, err)
			}
		case <-retentionTick.C:
			if err := p.sweepOldPosts(ctx); err != nil {
				p.log.Error("retention sweep failed", "err", err)
			}
		}
	}
}

// ── Performer sync ────────────────────────────────────────────────────

// Reddit URLs are recognised by parsing them, not by matching the
// string.
//
// A pattern anchored with (?:^|[./]) - the shape used elsewhere in this
// project - still accepts a path separator, so
// https://elsewhere.example/reddit.com/user/victim matched and the
// daemon polled a stranger's account on that performer's behalf. The
// host is the thing that decides whose account this is, so the host is
// what gets checked. The path pattern then only has to find the handle
// within a URL already known to be Reddit's.
var (
	reRedditUserPath = regexp.MustCompile(`(?i)^/(?:user|u)/([^/?#\s]+)`)
	reRedditSubPath  = regexp.MustCompile(`(?i)^/r/([^/?#\s]+)`)
)

// isRedditHost reports whether a host is reddit.com or a subdomain of
// it. Exact-or-dotted, so notreddit.com does not qualify.
func isRedditHost(host string) bool {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	return host == "reddit.com" || strings.HasSuffix(host, ".reddit.com")
}

// redditPathHandle extracts a handle from one URL, or "" if the URL is
// not a Reddit profile or subreddit.
func redditPathHandle(raw string, path *regexp.Regexp) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return ""
	}
	// A performer's urls[] is typed by hand, so a bare "reddit.com/u/x"
	// with no scheme is ordinary. url.Parse reads that as a path with
	// no host, which would fail the host check for the wrong reason.
	if u.Host == "" && !strings.HasPrefix(raw, "/") {
		if u2, err2 := url.Parse("https://" + raw); err2 == nil {
			u = u2
		}
	}
	if !isRedditHost(u.Hostname()) {
		return ""
	}
	m := path.FindStringSubmatch(u.EscapedPath())
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSuffix(m[1], "/")
}

func parseRedditHandle(urls []string) (string, string, bool) {
	var subHandle string
	for _, u := range urls {
		if h := redditPathHandle(u, reRedditUserPath); h != "" {
			return h, "user", true
		}
		if subHandle == "" {
			subHandle = redditPathHandle(u, reRedditSubPath)
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
	reported := 0
	for page := 1; ; page++ {
		performers, count, err := p.stash.FetchPerformersPage(ctx, page, pageSize)
		if err != nil {
			return err
		}
		reported = count
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
	// Stash says how many performers it has; anything short of that
	// means this sync did not see the whole library.
	//
	// Nothing checked, and the reconcile below deletes whatever it did
	// not see. One short page was enough: a library of 400 whose second
	// page came back empty left 200 unseen, and since the keep-set was
	// not empty and the removal was under half, both prune guards let
	// it through and a hundred performers and their posts went, logged
	// as a successful sync. That is the same failure the empty-response
	// guard was written for, one page down, and the likelier one: a
	// paginated API under load, a filter change, or a mid-page error
	// returned as an empty list all produce exactly this.
	if reported > 0 && total < reported {
		return fmt.Errorf(
			"stash reported %d performers but only %d arrived; not reconciling against a partial list",
			reported, total)
	}
	// Compared with the last sync that worked, not just with what this
	// one claimed.
	//
	// The check above only catches a page falling short of a high count.
	// A Stash that reports a low count and serves exactly that many rows
	// is internally consistent, so it sails through - and a reindex or a
	// mid-migration snapshot produces precisely that shape. Measuring
	// against the last good sync is what notices the library appearing
	// to shrink for reasons Stash is not telling us about.
	var lastTotal int
	_ = p.db.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM sync_state WHERE key='last_sync_total'`,
	).Scan(&lastTotal)
	if lastTotal > 0 && total*4 < lastTotal*3 {
		return fmt.Errorf(
			"stash returned %d performers where the last good sync saw %d; not reconciling against a shrunken list",
			total, lastTotal)
	}
	_, _ = p.db.ExecContext(ctx,
		`INSERT INTO sync_state(key,value) VALUES('last_sync_total', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(total))

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
		// A non-numeric id would silently become 0, and every such
		// performer would then collide on that primary key and
		// overwrite each other. Stash ids are numeric today; this
		// notices if that ever stops being true.
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
			INSERT INTO performers(stash_id, name, image_path, favorite, reddit_handle, handle_kind, handle_status, synced_at)
			VALUES(?,?,?,?,?,?, COALESCE((SELECT handle_status FROM performers WHERE stash_id=?), 'ok'), datetime('now'))
			ON CONFLICT(stash_id) DO UPDATE SET
				name=excluded.name,
				image_path=excluded.image_path,
				favorite=excluded.favorite,
				reddit_handle=excluded.reddit_handle,
				handle_kind=excluded.handle_kind,
				-- A changed handle deserves a fresh verdict. Status was
				-- left alone here, so a performer retired because their
				-- URL had a typo stayed retired after the user fixed it:
				-- only handles marked ok are polled, and nothing else
				-- resets one. Correcting the URL in Stash is now enough.
				handle_status=CASE
					WHEN performers.reddit_handle != excluded.reddit_handle
						OR performers.handle_kind != excluded.handle_kind
					THEN 'ok'
					ELSE performers.handle_status
				END,
				synced_at=datetime('now')`,
			id, perf.Name, perf.ImagePath, fav, handle, kind, id)
		if err != nil {
			return fmt.Errorf("upsert performer %d: %w", id, err)
		}
		keep[id] = true
	}
	return tx.Commit()
}

// safeToPrune reports whether a sync result is trustworthy enough to
// delete rows against.
//
// The keep-set is built only from what Stash returned, and a Stash that
// answers 200 with an empty performer list is not an error to any code
// path here. Stash mid-restart, mid-migration, or with changed filter
// semantics produces exactly that, and reconciling against it deleted
// every performer, which cascaded to every post: up to ninety days of
// Reddit history that cannot be re-fetched, because Reddit serves only
// the last 25 submissions per handle.
//
// Only the empty answer is refused outright, and it is refused forever,
// because it is never a real instruction: a Stash with no linked
// performers gives this daemon nothing to poll, so keeping the rows
// costs nothing and deleting them cannot help.
//
// A large-but-not-total removal is different. An earlier version
// refused those on a ratio, which turned out to be a trap: every kept
// performer is upserted before this runs, so `existing` is always
// keep+removing and the ratio reduced to "refuse whenever removed is at
// least kept". The inputs never change on the next cycle, so a library
// that genuinely shrank by half could never reconcile again, a two
// performer library could never drop to one, and because a failed sync
// stops the poll that follows it, saving config quietly stopped working
// too. It is now asked for once and confirmed by repetition: the same
// removal offered twice in a row is what the user meant.
// Removals up to this many go through on the first sync. Anything more
// waits to be asked twice.
//
// There used to be a ratio here instead, and a ratio has a cliff to walk
// under: a Stash reporting a transiently low count AND serving that many
// rows is internally consistent, so nothing upstream notices, and a
// reindex that hid 170 of 350 performers deleted them and their posts
// immediately because 170 is just under half. A small edit is someone
// removing a Reddit URL or two; anything bigger is worth ten minutes,
// since the daemon is a background job and nothing user-facing waits on
// it, while the posts it cascades away cannot be re-fetched.
const pruneWithoutAskingUpTo = 2

func safeToPrune(keep, existing, removing int, sameAsLastTime bool) (bool, string) {
	if existing == 0 {
		return true, ""
	}
	if keep == 0 {
		return false, "stash returned no linked performers at all"
	}
	if removing > pruneWithoutAskingUpTo && !sameAsLastTime {
		return false, "more than a couple of performers would go; waiting for the next sync to say the same"
	}
	return true, ""
}

// How long a proposed removal has to stand before a repeat of it
// counts as confirmation.
//
// Without an interval, "the same removal twice" is satisfied by two
// syncs seconds apart, and two of those are easy to come by: saving
// config wakes the poller, the wake loops when another save arrives
// while it runs, and the daily tick is a separate goroutine besides.
// So saving twice in a row defeated the guard. A removal that is real
// is still there in a few minutes.
const pruneConfirmAfter = 10 * time.Minute

// pruneWasOfferedBefore reports whether this exact removal was proposed
// far enough in the past to count as confirmed now.
func (p *Poller) pruneWasOfferedBefore(ctx context.Context, sig string) bool {
	var stored string
	if err := p.db.QueryRowContext(ctx,
		`SELECT value FROM sync_state WHERE key='pending_prune'`).Scan(&stored); err != nil {
		return false
	}
	// "<unix seconds>|<signature>"
	sep := strings.IndexByte(stored, '|')
	if sep < 0 || stored[sep+1:] != sig {
		return false
	}
	at, err := strconv.ParseInt(stored[:sep], 10, 64)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(at, 0)) >= pruneConfirmAfter
}

func (p *Poller) rememberPendingPrune(ctx context.Context, sig string) {
	// Only stamped when the proposal changes, or the clock would be
	// pushed forward on every refusal and the interval never elapse.
	var stored string
	_ = p.db.QueryRowContext(ctx,
		`SELECT value FROM sync_state WHERE key='pending_prune'`).Scan(&stored)
	if sep := strings.IndexByte(stored, '|'); sep >= 0 && stored[sep+1:] == sig {
		return
	}
	_, _ = p.db.ExecContext(ctx,
		`INSERT INTO sync_state(key,value) VALUES('pending_prune', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatInt(time.Now().Unix(), 10)+"|"+sig)
}

// pruneSignature identifies a proposed removal, so an unusually large
// one can be recognised when it is offered a second time.
func pruneSignature(toDelete []int) string {
	ids := append([]int(nil), toDelete...)
	sort.Ints(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
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
		// A sync with nothing to remove is the clearest possible
		// statement that any removal remembered from before no longer
		// applies. Leaving the row meant "the same removal twice in a
		// row" quietly became "twice, ever": a transient fault that
		// proposed a removal once, recovered for weeks, then recurred,
		// was accepted on that first recurrence.
		_, _ = p.db.ExecContext(ctx, `DELETE FROM sync_state WHERE key='pending_prune'`)
		return nil
	}
	var existing int
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performers`).Scan(&existing)
	sig := pruneSignature(toDelete)
	confirmed := p.pruneWasOfferedBefore(ctx, sig)
	if ok, why := safeToPrune(len(keep), existing, len(toDelete), confirmed); !ok {
		p.rememberPendingPrune(ctx, sig)
		p.log.Error("refusing to prune performers for now, treating this sync as failed",
			"reason", why,
			"linked_in_stash", len(keep),
			"in_db", existing,
			"would_remove", len(toDelete))
		return fmt.Errorf("refusing to prune performers: %s", why)
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM sync_state WHERE key='pending_prune'`)
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
	// Cookie expiry is the one failure users hit months after setup, and
	// on its own it is invisible: stories simply stop arriving. Record it
	// so /config can say so, rather than leaving it in a log nobody reads.
	expired := false
	succeeded := 0
	forbidden := 0
	var pending []statusMark
	// Verdicts are written at the end of the cycle now, which can take
	// minutes on a large library. If the config changed in the meantime,
	// they were reached under credentials nobody is using any more, and
	// writing them would quietly undo the revive that a cookie save just
	// performed.
	gen := p.configGeneration(ctx)
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
				expired = true
				break
			}
			if st := statusFor(err); st != "" {
				pending = append(pending, statusMark{stashID: t.StashID, status: st})
			}
			if errors.Is(err, reddit.ErrForbidden) {
				forbidden++
			}
			p.log.Warn("poll performer failed", "stash_id", t.StashID, "handle", t.Handle, "err", err)
			// Paced like a success. Failures used to skip this, which
			// only stayed harmless while a refused handle was retired
			// after one cycle: now that nothing is retired for being
			// refused, an unpaced run would fire every request in the
			// library back to back, on every tick, forever. That is the
			// behaviour most likely to keep an address refused.
			time.Sleep(perRequestSleep)
			continue
		}
		succeeded++
		time.Sleep(perRequestSleep)
	}
	// Only a fetch that actually worked clears the flag. Bailing out on a
	// rate limit before reaching anyone tells us nothing about the cookie,
	// and clearing on that would flap the warning off and on.
	// A handle is only suspended, missing or forbidden if Reddit was
	// answering everyone else at the same moment. When nothing succeeded,
	// the refusal is about the caller, not the performer: a dead cookie,
	// or an address Reddit will not serve. Writing it onto each handle in
	// turn would retire the whole library one tick at a time, and nothing
	// in the daemon ever sets a handle back to ok, so that damage
	// outlives its cause. Fixing the cookie afterwards would not bring a
	// single story back.
	// Reddit answering 403 to everything says nothing about any one
	// handle. It is what a dead cookie looks like, since an anonymous
	// caller is refused outright, and equally what an address Reddit
	// will not serve looks like. Either way it is about the caller, and
	// writing it onto each performer in turn retires the whole library
	// one tick at a time. Nothing else in the daemon sets a handle back
	// to ok, so that damage outlives its cause.
	//
	// Only 403 is ambiguous. A 404 or a suspension is a verdict on that
	// handle whatever else is going on, and holding those back would
	// leave a library of dead handles being re-polled forever with
	// nothing said about it.
	blanketRefusal := succeeded == 0 && forbidden > 0
	if p.configGeneration(ctx) != gen {
		p.log.Info("config changed mid-cycle, discarding this cycle's verdicts",
			"held_back", len(pending))
		pending = nil
	}
	for _, m := range pending {
		if m.status == "unavailable" && blanketRefusal {
			continue
		}
		p.markStatus(ctx, m.stashID, m.status)
	}
	if blanketRefusal {
		p.log.Error("reddit refused every request this cycle, so no handles were retired for it",
			"performers", len(targets))
	}

	// A blanket refusal is reported as an expired cookie because that is
	// the likely cause and the only one the user can act on. Telling
	// them instead that it is probably their address, which was the
	// first shape of this, steers them away from the fix that works in
	// the common case; the banner names the address as the fallback.
	p.setCookieExpired(ctx, expired || blanketRefusal, succeeded > 0)
	if succeeded > 0 {
		p.recordPollError(ctx, nil)
	}

	_, _ = p.db.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('last_poll', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().UTC().Format(time.RFC3339))
	p.log.Info("poll cycle done", "performers", len(targets), "new_posts", inserted, "elapsed", time.Since(start))
	return nil
}

// setCookieExpired records or clears the "reddit cookie is dead" marker
// in sync_state. Stored as the RFC3339 time it was first noticed, so the
// UI can say when stories stopped rather than just that they have.
func (p *Poller) setCookieExpired(ctx context.Context, expired, sawSuccess bool) {
	if expired {
		// First sighting wins: keep the original timestamp across ticks
		// so "since" does not reset to now on every failed cycle.
		_, _ = p.db.ExecContext(ctx, `INSERT OR IGNORE INTO sync_state(key,value)
			VALUES('reddit_cookie_expired_at', ?)`, time.Now().UTC().Format(time.RFC3339))
		return
	}
	if sawSuccess {
		_, _ = p.db.ExecContext(ctx, `DELETE FROM sync_state WHERE key='reddit_cookie_expired_at'`)
	}
}

// recordPollError keeps the last reason a cycle failed where /healthz
// can read it. A daemon that is running but achieving nothing looked
// identical to a healthy one from outside, which is most of why a
// broken setup is so hard to tell from a working one.
func (p *Poller) recordPollError(ctx context.Context, err error) {
	if err == nil {
		_, _ = p.db.ExecContext(ctx, `DELETE FROM sync_state WHERE key='last_poll_error'`)
		return
	}
	_, _ = p.db.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('last_poll_error', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, err.Error())
}

// configGeneration is bumped whenever credentials change. Compared
// across a poll cycle it says whether the answers gathered during it
// still describe the daemon as it is now.
func (p *Poller) configGeneration(ctx context.Context) string {
	var v string
	_ = p.db.QueryRowContext(ctx,
		`SELECT value FROM sync_state WHERE key='config_generation'`).Scan(&v)
	return v
}

// reviveRetiredOnce gives every retired handle one more chance, the
// first time a build carrying this runs.
//
// Older builds retired a performer on any 403, including the ones aimed
// at the caller rather than the handle, and nothing ever set one back.
// A library could therefore be whittled down to nothing over weeks, and
// the only outward sign was stories thinning out. Those installs cannot
// fix themselves: with every handle retired there is nobody left to
// poll, so no cycle can succeed, so nothing clears. Saving a cookie
// does it too, but only someone who suspects the cookie would think to,
// and for most people the cookie is fine.
//
// Once, not on every start: a handle that is genuinely gone should be
// allowed to stay gone after the next cycle retires it again.
func (p *Poller) reviveRetiredOnce(ctx context.Context) {
	const marker = "handles_revived_after_403_fix"
	var seen string
	_ = p.db.QueryRowContext(ctx,
		`SELECT value FROM sync_state WHERE key=?`, marker).Scan(&seen)
	if seen != "" {
		return
	}
	res, err := p.db.ExecContext(ctx,
		`UPDATE performers SET handle_status='ok' WHERE handle_status != 'ok'`)
	if err != nil {
		p.log.Warn("could not revive retired handles", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		p.log.Info("gave retired reddit handles another chance", "handles", n)
	}
	_, _ = p.db.ExecContext(ctx,
		`INSERT INTO sync_state(key,value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		marker, time.Now().UTC().Format(time.RFC3339))
}

// statusMark is a verdict on one handle, held back until the end of the
// cycle so it can be weighed against whether anything worked at all.
type statusMark struct {
	stashID int
	status  string
}

// statusFor maps a failure to the status it justifies recording against
// the handle, or "" when the failure says nothing about that handle in
// particular.
func statusFor(err error) string {
	switch {
	case errors.Is(err, reddit.ErrNotFound):
		return "notfound"
	case errors.Is(err, reddit.ErrSuspended):
		return "suspended"
	case errors.Is(err, reddit.ErrForbidden):
		return "unavailable"
	}
	return ""
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
		// Whether this retires the handle is the caller's call: it is the
		// only place that knows whether anyone else was answered.
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
	// The cutoff comes from the clock and the comparison is against
	// Reddit's own timestamp, so a container with a wrong clock, or one
	// restored from a snapshot, could put the cutoff past every row and
	// turn this into DELETE FROM posts.
	//
	// The bound has to be an upper one. A previous version bounded it
	// below, which reads sensibly and protects nothing: reaching past
	// every row needs a clock in the FUTURE, which yields a large
	// positive cutoff, while a clock in the past yields a negative one
	// that would have matched nothing anyway. Refuse to delete anything
	// dated later than the newest post actually held.
	// Sanity-checked against the daemon's own last successful poll
	// rather than against the newest post.
	//
	// Comparing with the newest post skipped the sweep whenever every
	// held post was older than the window, which is exactly the state a
	// daemon reaches when its Reddit cookie expires: retention went
	// quietly inert and the database stopped being bounded. The last
	// poll is the better reference because the cutoff should always sit
	// well before it; a clock jumped years forward puts it after.
	if cutoff <= 0 {
		p.log.Warn("retention cutoff is not sane, skipping sweep", "cutoff", cutoff)
		return nil
	}
	// Bounded by how much it would remove, not by the clock.
	//
	// Comparing the cutoff with the last poll looked like a clock check
	// and was not one: both come from the same clock, so a daemon that
	// is steadily fast stamps a fast last_poll too and the comparison
	// stays satisfied while the sweep deletes posts whose timestamps are
	// real and recent. Proportion cannot be fooled that way. A nightly
	// sweep of a ninety-day window removes about a day's worth; being
	// asked to remove most of the archive at once means something is
	// wrong with the clock or the data, and the archive is not
	// re-fetchable.
	var total, doomed int
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM posts`).Scan(&total)
	_ = p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM posts WHERE created_utc > 0 AND created_utc < ?`,
		cutoff).Scan(&doomed)
	if total > 20 && doomed*2 > total {
		p.log.Error("retention sweep would remove most of the archive, skipping it",
			"would_remove", doomed, "total", total, "cutoff", cutoff)
		return nil
	}
	// created_utc > 0 because a post whose timestamp Reddit omitted
	// stores zero, and zero is older than every cutoff. Those were
	// deleted within a day of arriving and silently re-fetched on the
	// next poll, forever.
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM posts WHERE created_utc > 0 AND created_utc < ?`, cutoff)
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
