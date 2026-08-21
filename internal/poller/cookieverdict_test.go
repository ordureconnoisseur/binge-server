package poller

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/binge-server/internal/reddit"
)

// The cookie-expired marker is what the UI shows the user, and acting on
// it means pasting a fresh cookie out of a browser. It has to mean the
// cookie, and nothing else.

func markerSet(t *testing.T, p *Poller) bool {
	t.Helper()
	var n int
	err := p.db.QueryRow(
		`SELECT COUNT(*) FROM sync_state WHERE key='reddit_cookie_expired_at'`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return n > 0
}

func TestACycleThatFetchedAnythingDoesNotCondemnTheCookie(t *testing.T) {
	p := testPoller(t)

	// One handle said the cookie was bad; plenty of others worked. A
	// cookie that fetched anything at all is not expired, and this used
	// to write the marker anyway because the expired branch returned
	// before it consulted sawSuccess.
	p.setCookieExpired(context.Background(), true, true)

	if markerSet(t, p) {
		t.Error("marked the cookie expired on a cycle that fetched posts")
	}
}

func TestACycleThatFetchedNothingStillCondemnsTheCookie(t *testing.T) {
	p := testPoller(t)

	// The case the marker exists for: nothing came back at all.
	p.setCookieExpired(context.Background(), true, false)

	if !markerSet(t, p) {
		t.Error("a cycle that fetched nothing left the cookie unmarked")
	}
}

func TestARedirectIsAHandleVerdictNotACookieVerdict(t *testing.T) {
	// A renamed or merged community redirects, and so does the over18
	// interstitial. Mapping that to the cookie meant one such handle
	// abandoned every performer after it and told the user their cookie
	// had died.
	if got := statusFor(reddit.ErrRedirected); got == "" {
		t.Error("a redirect records no handle status, so last_polled_at " +
			"never advances and the handle sorts first forever")
	}
}
