package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/db"
)

// Can a daemon reached over the public internet ever be configured?
//
// The auth middleware refuses public IPs until a Stash API key is stored,
// and the only way to store one is a request that the same rule refuses.
func TestFirstRunFromPublicIP(t *testing.T) {
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store, err := configstore.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: conn, store: store}

	call := func(path, remote string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"stashApiKey":"k"}`))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Error
	}

	// Baseline: from the LAN, first-run config is reachable.
	if code, _ := call("/config", "192.168.1.50:5000"); code != http.StatusOK {
		t.Errorf("LAN first-run POST /config = %d, want 200", code)
	}

	// The reported case: the browser reaches the daemon over the public
	// internet (reverse proxy, tunnel, port-forward). A first-run write
	// carrying a Stash API key must reach the handler, which probes the
	// key before storing anything.
	if code, msg := call("/config", "203.0.113.9:5000"); code != http.StatusOK {
		t.Errorf("public first-run POST /config = %d %q, want it to reach the handler", code, msg)
	}

	// Every other route stays refused, so the exemption is a doorway to
	// the config handler and not a general bypass.
	for _, path := range []string{"/reddit/stories", "/save", "/reddit/refresh"} {
		if code, _ := call(path, "203.0.113.9:5000"); code != http.StatusForbidden {
			t.Errorf("public %s = %d, want 403", path, code)
		}
	}

	// GET /config is not the claiming request and stays refused, so an
	// unconfigured daemon does not report its state to the internet.
	{
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.RemoteAddr = "203.0.113.9:5000"
		rec := httptest.NewRecorder()
		s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("public GET /config = %d, want 403", rec.Code)
		}
	}

	// And /healthz stays open, which is why the card still says
	// "Connected" while every write is refused.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	t.Logf("public-IP GET /healthz -> %d", rec.Code)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 (it is the exempt route)", rec.Code)
	}
}

// The exemption must not become a way to occupy a daemon you have not
// proved you own: a first-run write from a public address that sets
// anything other than the Stash API key is refused by the handler.
func TestFirstRunPublicWriteMustSetStashKey(t *testing.T) {
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store, err := configstore.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: conn, store: store}

	post := func(body, remote string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.postConfig(rec, req)
		var out struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Error
	}

	code, msg := post(`{"redditSessionCookie":"reddit_session=x"}`, "203.0.113.9:5000")
	if code != http.StatusForbidden {
		t.Errorf("public cookie-only first-run write = %d %q, want 403", code, msg)
	}
	if store.Get(configstore.KeyRedditCookie) != "" {
		t.Error("cookie was persisted by a request that proved nothing")
	}

	// The same request from the LAN is ordinary first-run setup and is
	// not subject to the rule, so this does not regress local installs.
	// (It still fails validation against a nonexistent reddit session,
	// which is a different gate; only the 403 must be gone.)
	if code, _ := post(`{"redditSessionCookie":"reddit_session=x"}`, "192.168.1.50:5000"); code == http.StatusForbidden {
		t.Error("LAN first-run write hit the public-address rule")
	}
}
