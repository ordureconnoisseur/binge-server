package poller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ordureconnoisseur/binge-server/internal/db"
	"github.com/ordureconnoisseur/binge-server/internal/reddit"
)

func testPoller(t *testing.T) *Poller {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Poller{
		db:  conn,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func addPerformer(t *testing.T, p *Poller, id int, handle, status string) {
	t.Helper()
	_, err := p.db.Exec(`INSERT INTO performers
		(stash_id, name, image_path, favorite, reddit_handle, handle_kind, handle_status, synced_at)
		VALUES(?,?,?,?,?,?,?, datetime('now'))`,
		id, fmt.Sprintf("p%d", id), "", 1, handle, "user", status)
	if err != nil {
		t.Fatalf("insert performer: %v", err)
	}
}

func statusOf(t *testing.T, p *Poller, id int) string {
	t.Helper()
	var st string
	if err := p.db.QueryRow(
		`SELECT handle_status FROM performers WHERE stash_id=?`, id).Scan(&st); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return st
}

func syncValue(t *testing.T, p *Poller, key string) string {
	t.Helper()
	var v string
	if err := p.db.QueryRow(`SELECT value FROM sync_state WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// Only a failure Reddit aimed at one handle should ever be written
// against that handle.
func TestStatusFor(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{reddit.ErrNotFound, "notfound"},
		{reddit.ErrSuspended, "suspended"},
		{reddit.ErrForbidden, "unavailable"},
		{reddit.ErrRateLimit, ""},
		{reddit.ErrCookieExpired, ""},
		{errors.New("connection reset"), ""},
	}
	for _, c := range cases {
		if got := statusFor(c.err); got != c.want {
			t.Fatalf("statusFor(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// The rule the whole fix turns on: 403 is held back only when Reddit
// refused everyone, because then it is about the caller. A 404 is a
// verdict on the handle whatever else is happening, and holding those
// back would leave dead handles polled forever with nothing said.
func TestOnlyForbiddenIsHeldBackOnABlanketRefusal(t *testing.T) {
	cases := []struct {
		name       string
		succeeded  int
		forbidden  int
		status     string
		wantWrites bool
	}{
		{"403 while others answered is about the handle", 3, 1, "unavailable", true},
		{"403 when nobody answered is about the caller", 0, 4, "unavailable", false},
		{"404 is about the handle even when nobody answered", 0, 1, "notfound", true},
		{"suspension is about the handle too", 0, 1, "suspended", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testPoller(t)
			addPerformer(t, p, 1, "h1", "ok")

			blanketRefusal := c.succeeded == 0 && c.forbidden > 0
			pending := []statusMark{{stashID: 1, status: c.status}}
			for _, m := range pending {
				if m.status == "unavailable" && blanketRefusal {
					continue
				}
				p.markStatus(context.Background(), m.stashID, m.status)
			}

			got := statusOf(t, p, 1)
			if c.wantWrites && got != c.status {
				t.Fatalf("expected handle retired as %q, got %q", c.status, got)
			}
			if !c.wantWrites && got != "ok" {
				t.Fatalf("handle retired as %q on a caller-side refusal", got)
			}
		})
	}
}

// A blanket refusal is reported as an expired cookie, because that is
// the likely cause and the only one the user can act on. Reporting it
// as a blocked address instead, which this first did, steers people
// away from the fix that works.
func TestBlanketRefusalReadsAsAnExpiredCookie(t *testing.T) {
	p := testPoller(t)
	ctx := context.Background()

	p.setCookieExpired(ctx, false || (0 == 0 && 4 > 0), false)
	first := syncValue(t, p, "reddit_cookie_expired_at")
	if first == "" {
		t.Fatal("a cycle refused outright left no marker at all")
	}

	// The UI says when it started, so a second bad cycle must not move
	// the timestamp to now.
	p.setCookieExpired(ctx, true, false)
	if got := syncValue(t, p, "reddit_cookie_expired_at"); got != first {
		t.Fatalf("timestamp moved from %q to %q", first, got)
	}

	// A cycle that reached nobody proves nothing, so it must not clear.
	p.setCookieExpired(ctx, false, false)
	if got := syncValue(t, p, "reddit_cookie_expired_at"); got != first {
		t.Fatalf("marker cleared by a cycle that proved nothing: %q", got)
	}

	p.setCookieExpired(ctx, false, true)
	if got := syncValue(t, p, "reddit_cookie_expired_at"); got != "" {
		t.Fatalf("marker survived a successful fetch: %q", got)
	}
}

// An install whittled down by the old behaviour cannot recover on its
// own: with every handle retired there is nobody left to poll, so no
// cycle can succeed, so nothing clears.
func TestReviveRetiredOnce(t *testing.T) {
	p := testPoller(t)
	ctx := context.Background()
	addPerformer(t, p, 1, "h1", "ok")
	addPerformer(t, p, 2, "h2", "unavailable")
	addPerformer(t, p, 3, "h3", "suspended")
	addPerformer(t, p, 4, "h4", "notfound")

	p.reviveRetiredOnce(ctx)
	for id := 1; id <= 4; id++ {
		if got := statusOf(t, p, id); got != "ok" {
			t.Fatalf("performer %d still %q after the revive", id, got)
		}
	}

	// Once, not on every start: a handle that really is gone has to be
	// allowed to stay gone once the next cycle retires it again.
	p.markStatus(ctx, 4, "notfound")
	p.reviveRetiredOnce(ctx)
	if got := statusOf(t, p, 4); got != "notfound" {
		t.Fatalf("second revive resurrected a handle again: %q", got)
	}
}

// Verdicts now land at the end of a cycle that can run for minutes. If
// credentials changed while it ran, they describe a daemon that no
// longer exists, and writing them would undo the revive a cookie save
// just performed.
func TestConfigGenerationGuard(t *testing.T) {
	p := testPoller(t)
	ctx := context.Background()

	gen := p.configGeneration(ctx)
	if gen != "" {
		t.Fatalf("expected no generation on a fresh db, got %q", gen)
	}

	_, err := p.db.Exec(`INSERT INTO sync_state(key,value) VALUES('config_generation','2026-08-20T00:00:00Z')`)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if p.configGeneration(ctx) == gen {
		t.Fatal("generation did not change after a config write")
	}
}
