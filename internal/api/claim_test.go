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

// The first-run doorway lets a public caller claim an unconfigured daemon
// by presenting a Stash API key the daemon can prove works. These cover
// the ways a caller could satisfy the probe while holding no key at all,
// which would turn the doorway into a remote takeover.

func newClaimServer(t *testing.T) *Server {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store, err := configstore.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{db: conn, store: store}
}

func claim(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5000" // the public internet
	rec := httptest.NewRecorder()
	s.postConfig(rec, req)
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Error
}

// A Stash with authentication switched off answers any key with 200, so
// "the probe passed" says nothing about whether the caller holds a
// credential. Without this, anyone who can reach an unconfigured daemon
// owns it.
func TestClaimRefusedWhenStashHasNoAuth(t *testing.T) {
	authless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":{"version":"v0.30.0"}}}`))
	}))
	defer authless.Close()

	s := newClaimServer(t)
	code, msg := claim(t, s, `{"stashUrl":"`+authless.URL+`","stashApiKey":"attacker-chosen"}`)
	if code == http.StatusOK {
		t.Errorf("claimed an unconfigured daemon against an authless Stash: %d %q", code, msg)
	}
	if got := s.store.Get(configstore.KeyStashAPIKey); got != "" {
		t.Errorf("stored attacker key %q", got)
	}
}

// A Stash that actually checks the key must still let its owner in, or
// the guard above has broken ordinary setup.
func TestClaimAllowedAgainstRealStash(t *testing.T) {
	const real = "REAL-KEY"
	stash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("ApiKey") != real {
			_, _ = w.Write([]byte(`{"errors":[{"message":"unauthorized"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"version":{"version":"v0.30.0"}}}`))
	}))
	defer stash.Close()

	s := newClaimServer(t)
	code, msg := claim(t, s, `{"stashUrl":"`+stash.URL+`","stashApiKey":"`+real+`"}`)
	if code != http.StatusOK {
		t.Fatalf("owner with a real key was refused: %d %q", code, msg)
	}
	if s.store.Get(configstore.KeyStashAPIKey) != real {
		t.Error("real key was not stored")
	}
}

// stashURLAllowed inspects only the hostname, and the probe appends
// "/graphql" by string concatenation, so a query string can aim the probe
// at any path on a private host, including the daemon's own /config,
// which answers 200 to a body it does not understand.
func TestClaimRefusesStashURLWithPathOrQuery(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:7878/config?x=",
		"http://127.0.0.1:7878/config#",
		"http://localhost:9999/some/path",
	} {
		s := newClaimServer(t)
		code, msg := claim(t, s, `{"stashUrl":"`+raw+`","stashApiKey":"attacker-chosen"}`)
		if code == http.StatusOK {
			t.Errorf("accepted stashUrl %q: %d %q", raw, code, msg)
		}
	}
}

// probeRedditCookie refuses to follow redirects; probeStashAPIKey did
// not. Go strips Authorization and Cookie across hosts but not a custom
// header, so the real Stash key travelled to wherever the redirect
// pointed, including a public host.
func TestStashProbeDoesNotFollowRedirects(t *testing.T) {
	leaked := make(chan string, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("ApiKey")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer collector.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+"/graphql", http.StatusFound)
	}))
	defer redirector.Close()

	err := probeStashAPIKey(t.Context(), redirector.URL, "REAL-STASH-KEY")
	select {
	case got := <-leaked:
		t.Errorf("probe followed a redirect and handed the key to it: %q", got)
	default:
	}
	if err == nil {
		t.Error("probe reported success for a host that only redirected")
	}
}
