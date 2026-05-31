package api

import (
	"net"
	"net/url"
	"strings"
)

// parseOrigins splits a comma-separated BINGE_ALLOWED_ORIGIN value into a
// normalised allowlist. A literal "*" is intentionally dropped — wildcard
// CORS on a credential-writing API is unsafe, so "*" now means "no
// cross-origin browser access" (loopback is still permitted). Cross-host
// setups must name their Stash origin(s) explicitly.
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
// An empty Origin (curl, the native iOS app, server-to-server) is allowed
// — CORS/CSRF only applies to browsers, which always send Origin on
// cross-origin requests. A loopback Origin is always allowed (same-host
// Stash). Otherwise the Origin must exactly match a configured entry.
func originAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	if isLoopbackOrigin(origin) {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimRight(origin, "/"), a) {
			return true
		}
	}
	return false
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
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
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	// Local + tailnet hostnames.
	if host == "localhost" ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".ts.net") {
		return true
	}
	// A bare hostname (no dot) is a LAN/tailnet machine name, not a
	// public FQDN — allow it.
	if !strings.Contains(host, ".") {
		return true
	}
	// IP literals: loopback, RFC1918 private, or Tailscale CGNAT.
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || isCGNAT(ip)
	}
	// A dotted hostname that isn't a recognised local/tailnet suffix is
	// treated as public and rejected.
	return false
}

var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

func isCGNAT(ip net.IP) bool {
	return cgnatNet != nil && cgnatNet.Contains(ip)
}
