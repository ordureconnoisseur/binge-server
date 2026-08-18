package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

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
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "missing or invalid Stash API key",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
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
