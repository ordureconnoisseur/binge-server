package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/db"
)

// The Reddit cookie dies every few months and the only symptom is that
// stories stop arriving, so /config has to report it. These cover the
// three states the settings card distinguishes.
func newConfigServer(t *testing.T) *Server {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store, err := configstore.New(conn)
	if err != nil {
		t.Fatalf("configstore.New: %v", err)
	}
	return &Server{db: conn, store: store}
}

func getConfigResponse(t *testing.T, s *Server) configGetResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.getConfig(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got configGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestConfigReportsExpiredRedditCookie(t *testing.T) {
	s := newConfigServer(t)
	if err := s.store.Set(configstore.KeyRedditCookie, "reddit_session=abc"); err != nil {
		t.Fatal(err)
	}

	// Healthy: cookie stored, poller has not complained.
	if got := getConfigResponse(t, s).RedditCookieExpiredAt; got != "" {
		t.Errorf("fresh cookie reported expired at %q", got)
	}

	// The poller writes this row when reddit rejects the session.
	const when = "2026-08-18T09:00:00Z"
	if _, err := s.db.Exec(
		`INSERT INTO sync_state(key,value) VALUES('reddit_cookie_expired_at', ?)`, when,
	); err != nil {
		t.Fatal(err)
	}
	if got := getConfigResponse(t, s).RedditCookieExpiredAt; got != when {
		t.Errorf("RedditCookieExpiredAt = %q, want %q", got, when)
	}
}

// A daemon that never had a cookie must read as "not set up" rather than
// "expired", or a first-run install greets you with a stale-credential
// warning for a credential you have not entered yet.
func TestConfigHidesExpiryWhenNoCookieStored(t *testing.T) {
	s := newConfigServer(t)
	if _, err := s.db.Exec(
		`INSERT INTO sync_state(key,value) VALUES('reddit_cookie_expired_at', ?)`,
		"2026-08-18T09:00:00Z",
	); err != nil {
		t.Fatal(err)
	}
	got := getConfigResponse(t, s)
	if got.RedditCookieSet {
		t.Error("RedditCookieSet true with no cookie stored")
	}
	if got.RedditCookieExpiredAt != "" {
		t.Errorf("RedditCookieExpiredAt = %q, want empty", got.RedditCookieExpiredAt)
	}
}
