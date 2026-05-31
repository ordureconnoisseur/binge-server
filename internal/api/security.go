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
func stashURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isPrivateHost(u.Hostname())
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
