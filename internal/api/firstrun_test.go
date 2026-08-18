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
	// internet (reverse proxy, tunnel, port-forward).
	code, msg := call("/config", "203.0.113.9:5000")
	t.Logf("public-IP first-run POST /config -> %d %q", code, msg)
	if code == http.StatusOK {
		t.Skip("no deadlock: public first-run is permitted")
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
