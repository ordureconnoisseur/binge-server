package api

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/configstore"
)

// Authentication, deliberately mirroring Stash's own posture — no weaker,
// and no more elaborate.
//
// Stash's model (internal/api/authentication.go):
//   - No credentials configured: serve the request, but refuse public IPs
//     ("Stash cannot be accessed from public IPs when authentication is
//     not configured").
//   - Credentials configured: require them. An API key is accepted via the
//     `ApiKey` header or an `apikey` query parameter.
//
// binge-server's equivalent of "credentials configured" is holding a Stash
// API key, because that key IS the credential checked against — the same
// secret, so nothing new to issue, store or rotate.
//
// Why this exists now: the daemon used to bind to loopback, where "any
// local process may call us" was a reasonable stance. It can now be bound
// LAN-wide (the browser is often not on the Stash host), and at that point
// an unauthenticated /config lets anyone on the network rewrite the stored
// credentials — including pointing stashUrl at their own machine and
// collecting the API key on the next poll.

const (
	// Stash's spellings, so a caller that can already talk to Stash needs
	// no new vocabulary.
	apiKeyHeader = "ApiKey"
	apiKeyParam  = "apikey"
)

// authMiddleware gates every route except the liveness probe.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz stays open: it's the container HEALTHCHECK and the
		// install UI's "is it up yet" poll, both of which run before any
		// credential exists. It exposes no secrets — booleans and counts.
		// Stash exempts its login and asset paths on the same reasoning.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		want := s.store.Get(configstore.KeyStashAPIKey)
		if want == "" {
			// Nothing to check against yet: a fresh install, or a Stash
			// with authentication switched off. Match Stash exactly —
			// private networks may proceed, public IPs may not. This is
			// also what keeps first-run setup possible: POST /config has
			// to be reachable before there is a key to present.
			if !requestFromPrivateIP(r) {
				// One exception, or this is a dead end: every route is
				// refused until a key is stored, and storing one is a
				// request. Reaching the daemon through a tunnel or a
				// reverse proxy puts a public address in RemoteAddr, so
				// an ordinary remote setup lands here on first run with
				// no way forward.
				//
				// Claiming an unconfigured daemon this way requires
				// presenting a Stash API key that the daemon can prove
				// works, against a Stash URL that stashURLAllowed has
				// already restricted to its own private network. An
				// attacker on the internet cannot satisfy that without
				// already holding a working key for a Stash they cannot
				// reach; postConfig enforces it.
				if r.Method == http.MethodPost && r.URL.Path == "/config" {
					next.ServeHTTP(w, r)
					return
				}
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "binge-server cannot be reached from a public IP until a Stash API key is configured. " +
						"Open binge from your Stash and let it send the key, or seed one with the STASH_API_KEY environment variable.",
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if !credentialMatches(r, want) {
			// Rotating the key in Stash used to lock the daemon out for
			// good. The browser holds the new key, the daemon still
			// wants the old one, so the write that would fix it needs
			// the credential it is trying to replace. Every route
			// answered 401 including /config, and the only way back was
			// to delete the database and lose every stored post.
			//
			// A caller from the local network holding a key that this
			// daemon's own Stash accepts has exactly the authority the
			// stored key was standing in for, so let that through to
			// POST /config and no further. It proves the claim against
			// Stash rather than believing it: an attacker would need a
			// working key for a Stash they must already be positioned
			// to reach.
			if s.allowKeyRotation(r) {
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "missing or invalid Stash API key. If you rotated the key in Stash, " +
					"open binge from that Stash on the same network and it will re-send it.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowKeyRotation reports whether a request presenting the wrong key
// should still be allowed to replace it.
//
// Deliberately narrow. Only POST /config, only from a private address,
// and only when the presented key is one the configured Stash itself
// accepts. The last condition is the load-bearing one: it is checked
// against the Stash this daemon is already pointed at, whose address
// stashURLAllowed has restricted to the local network, so it cannot be
// satisfied by someone who is not already there.
func (s *Server) allowKeyRotation(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/config" {
		return false
	}
	if !requestFromPrivateIP(r) {
		return false
	}
	presented := presentedCredential(r)
	if presented == "" {
		return false
	}
	// Two requests to Stash per attempt, triggerable by anyone on the
	// local network who can present a wrong key. Cheap to allow now and
	// then, not worth allowing in a loop.
	s.rotateMu.Lock()
	if time.Since(s.lastRotateTry) < 2*time.Second {
		s.rotateMu.Unlock()
		return false
	}
	s.lastRotateTry = time.Now()
	s.rotateMu.Unlock()
	stashURL := s.store.Get(configstore.KeyStashURL)
	if stashURL == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if err := probeStashAPIKey(ctx, stashURL, presented); err != nil {
		return false
	}
	// An authless Stash accepts anything, so a pass there proves
	// nothing. Same negative control the first-run claim uses.
	if err := probeStashAPIKey(ctx, stashURL, bogusProbeKey); err == nil {
		return false
	}
	return true
}

// credentialMatches accepts the key the way Stash does, plus Bearer.
//
// The query parameter is not a convenience: /pornhub/stream, /reddit/proxy
// and /redgifs/proxy are consumed as <video src> and <img src>, and a
// browser cannot attach headers to those. Stash supports `apikey` for the
// same reason on its own media routes.
func credentialMatches(r *http.Request, want string) bool {
	if v := r.Header.Get(apiKeyHeader); v != "" {
		return secureEqual(v, want)
	}
	if v := r.Header.Get("Authorization"); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return secureEqual(strings.TrimSpace(after), want)
		}
	}
	if v := r.URL.Query().Get(apiKeyParam); v != "" {
		return secureEqual(v, want)
	}
	return false
}

// presentedCredential returns the key the caller sent, in the same
// three places credentialMatches looks for it.
func presentedCredential(r *http.Request) string {
	if v := r.Header.Get(apiKeyHeader); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return r.URL.Query().Get(apiKeyParam)
}

func secureEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// requestFromPrivateIP reports whether the peer is on a local network.
// Deliberately reads RemoteAddr only: honouring X-Forwarded-For would let
// any caller claim to be local by setting a header, and binge-server is
// not meant to sit behind a proxy.
func requestFromPrivateIP(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	// Strip an IPv6 zone (fe80::1%eth0).
	if i := strings.Index(host, "%"); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		isCGNAT(ip)
}
