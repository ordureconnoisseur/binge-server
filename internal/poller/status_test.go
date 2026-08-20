package poller

import (
	"context"
	"errors"
	"fmt"
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
	return &Poller{db: conn}
}

func syncValue(t *testing.T, p *Poller, key string) string {
	t.Helper()
	var v string
	err := p.db.QueryRow(`SELECT value FROM sync_state WHERE key=?`, key).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

// Only a failure that Reddit aimed at one handle should ever be written
// against that handle. Everything else is about the caller.
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

// The marker exists so a blanket refusal is visible in the UI instead of
// stories quietly stopping, which is how the same class of failure went
// unnoticed with an expired cookie.
func TestRedditBlockedMarker(t *testing.T) {
	p := testPoller(t)
	ctx := context.Background()

	p.setRedditBlocked(ctx, false, false)
	if got := syncValue(t, p, "reddit_blocked_at"); got != "" {
		t.Fatalf("marker set with nothing wrong: %q", got)
	}

	p.setRedditBlocked(ctx, true, false)
	first := syncValue(t, p, "reddit_blocked_at")
	if first == "" {
		t.Fatal("marker not set after a blocked cycle")
	}

	// The UI says when it started, so a second bad cycle must not move
	// the timestamp forward to now.
	p.setRedditBlocked(ctx, true, false)
	if got := syncValue(t, p, "reddit_blocked_at"); got != first {
		t.Fatalf("timestamp moved from %q to %q", first, got)
	}

	// A cycle that reached nobody says nothing either way, so it must
	// not clear the warning.
	p.setRedditBlocked(ctx, false, false)
	if got := syncValue(t, p, "reddit_blocked_at"); got != first {
		t.Fatalf("marker cleared by a cycle that proved nothing: %q", got)
	}

	p.setRedditBlocked(ctx, false, true)
	if got := syncValue(t, p, "reddit_blocked_at"); got != "" {
		t.Fatalf("marker survived a successful fetch: %q", got)
	}
}

// The bug this guards: a run of refusals used to retire every performer
// one at a time, and nothing in the daemon ever set one back to ok, so
// fixing the cookie afterwards brought nothing back.
func TestRetiredHandlesAreRevivable(t *testing.T) {
	p := testPoller(t)
	for i, status := range []string{"ok", "unavailable", "suspended", "notfound"} {
		_, err := p.db.Exec(`INSERT INTO performers
			(stash_id, name, image_path, favorite, reddit_handle, handle_kind, handle_status, synced_at)
			VALUES(?,?,?,?,?,?,?, datetime('now'))`,
			i+1, fmt.Sprintf("p%d", i), "", 1, fmt.Sprintf("h%d", i), "user", status)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	var pollable int
	_ = p.db.QueryRow(`SELECT COUNT(*) FROM performers WHERE handle_status='ok'`).Scan(&pollable)
	if pollable != 1 {
		t.Fatalf("expected 1 pollable performer to start, got %d", pollable)
	}

	// The statement the config handler runs when a new cookie is saved.
	if _, err := p.db.Exec(
		`UPDATE performers SET handle_status='ok' WHERE handle_status != 'ok'`,
	); err != nil {
		t.Fatalf("revive: %v", err)
	}

	_ = p.db.QueryRow(`SELECT COUNT(*) FROM performers WHERE handle_status='ok'`).Scan(&pollable)
	if pollable != 4 {
		t.Fatalf("expected all 4 pollable after a new cookie, got %d", pollable)
	}
}
