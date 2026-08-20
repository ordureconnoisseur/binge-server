package stash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The Stash API key must never travel to a host the user did not
// configure.
//
// Go strips Authorization and Cookie when a redirect crosses hosts, but
// a custom header like ApiKey is copied wherever the redirect points.
// This client carries that key on every poll, sync and save, and the
// key doubles as the daemon's own credential, so a single 3xx from the
// configured Stash host was enough to hand over both. Restricting the
// Stash URL to private addresses does nothing about it, because the
// redirect is followed after that check has passed.
func TestAPIKeyDoesNotFollowRedirects(t *testing.T) {
	var (
		mu       sync.Mutex
		leakedTo []string
	)

	// Stands in for anywhere a redirect might point: an attacker's box,
	// or any public host.
	elsewhere := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			leakedTo = append(leakedTo, r.Header.Get("ApiKey"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}))
	defer elsewhere.Close()

	// Stands in for the configured Stash, which has been compromised,
	// misconfigured, or is simply answering a redirect.
	redirector := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/graphql", http.StatusFound)
		}))
	defer redirector.Close()

	c := New(redirector.URL, "SENTINEL-KEY")
	var out struct{}
	// The error is irrelevant: what matters is where the key went.
	_ = c.do(context.Background(), `query { __typename }`, nil, &out)

	mu.Lock()
	defer mu.Unlock()
	for _, got := range leakedTo {
		if got != "" {
			t.Fatalf("api key delivered to a redirect target: %q", got)
		}
	}
}
