package api

import (
	"net"
	"net/url"
	"strings"
)

// parseOrigins splits a comma-separated BINGE_ALLOWED_ORIGIN value into a
// normalised allowlist. A literal "*" is intentionally dropped — wildcard
// CORS on a credential-writing API is unsafe. This list is only needed for
// PUBLIC Stash origins (a real domain behind a reverse proxy); loopback /
// private / tailnet origins are allowed automatically (see originAllowed).
func parseOrigins(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p != "" && p != "*" {
			out = append(out, p)
		}
	}
	return out
}

// originAllowed reports whether a request's Origin header may be honoured.
// Allowed when:
//   - there's no Origin (curl / the native iOS app / server-to-server —
//     CORS/CSRF is a browser-only concern), or
//   - the Origin's host is loopback / private / tailnet (a self-hosted
//     Stash on a trusted network — the common case, ZERO config), or
//   - the Origin exactly matches a configured allowlist entry (needed only
//     when Stash is served from a PUBLIC origin, e.g. a real domain).
//
// A malicious public web page (https://evil.com) has a public Origin and
// matches none of these, so browser CSRF stays blocked.
func originAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	if isPrivateOrigin(origin) {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimRight(origin, "/"), a) {
			return true
		}
	}
	return false
}

func isPrivateOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isPrivateHost(u.Hostname())
}

// stashURLAllowed restricts the Stash destination to loopback / private /
// tailnet hosts. This is the key defence against credential exfiltration:
// even a config write can't repoint the stored Stash API key at a public
// attacker-controlled host, because the daemon refuses to send it anywhere
// but the local network / tailnet.
func stashURLAllowed(raw string) bool { return stashURLProblem(raw) == "" }

// stashURLProblem returns a sentence naming what is wrong with a Stash
// URL, or "" when nothing is.
//
// Three quite different mistakes used to share one message about
// loopback and private addresses. The commonest by far is pasting the
// GraphQL endpoint, which people copy out of Stash's own playground,
// and being told that http://localhost:9999/graphql is not a local
// address sends them to look at their network instead of their URL.
func stashURLProblem(raw string) string {
	const needsScheme = "stashUrl needs a scheme, for example http://localhost:9999"
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse rejects "192.168.1.5:9999" outright, reading the
		// leading digits as a scheme, so the numeric hosts most people
		// type got the least useful message of the three. If it parses
		// once a scheme is added, a scheme is what it was missing.
		if _, err2 := url.Parse("http://" + raw); err2 == nil {
			return needsScheme
		}
		return "stashUrl is not a URL"
	}
	if u.Scheme == "" {
		return needsScheme
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "stashUrl must start with http:// or https://"
	}
	// Only a bare origin. Callers build a target by appending a path to
	// this value, so a query or fragment would swallow that path and a
	// non-root path would move it: "http://127.0.0.1:7878/config?x="
	// plus "/graphql" is a request to /config on the daemon itself,
	// which answers 200 to a body it does not understand and so passes
	// for a working Stash.
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "stashUrl should be just the address of your Stash, with no query or login part"
	}
	if u.Path != "" && u.Path != "/" {
		if strings.EqualFold(strings.TrimRight(u.Path, "/"), "/graphql") {
			return "stashUrl should be the address of Stash itself, not its GraphQL endpoint: drop the /graphql"
		}
		return "stashUrl should be the address of Stash itself, with nothing after the port"
	}
	if !isPrivateHost(u.Hostname()) {
		return "stashUrl must be a loopback, private, or tailnet address"
	}
	return ""
}

// isPrivateHost reports whether a hostname is loopback, an RFC1918 private
// IP, Tailscale CGNAT (100.64/10), a .local/.internal/.ts.net name, or a
// bare LAN hostname (no dot). Public IPs and public FQDNs return false.
func isPrivateHost(host string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	// IP literal? Classify by range (handles IPv4 + IPv6).
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || isCGNAT(ip)
	}
	// Local / tailnet hostnames.
	if host == "localhost" ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".ts.net") {
		return true
	}
	// A bare hostname (no dot) is a LAN/tailnet machine name, not a
	// public FQDN — allow it.
	return !strings.Contains(host, ".")
}

var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

func isCGNAT(ip net.IP) bool {
	return cgnatNet != nil && cgnatNet.Contains(ip)
}
