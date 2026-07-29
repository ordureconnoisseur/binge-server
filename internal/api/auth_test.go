package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestFromPrivateIP(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:5000", true},
		{"192.168.1.20:5000", true},
		{"10.0.0.5:5000", true},
		{"100.101.45.18:5000", true},   // tailscale CGNAT
		{"[::1]:5000", true},
		{"203.0.113.7:5000", false},    // public
		{"8.8.8.8:5000", false},
		{"garbage", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/config", nil)
		r.RemoteAddr = c.addr
		if got := requestFromPrivateIP(r); got != c.want {
			t.Errorf("requestFromPrivateIP(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestCredentialMatches(t *testing.T) {
	const key = "s3cret-key"
	mk := func(f func(*http.Request)) *http.Request {
		r := httptest.NewRequest("GET", "/config", nil)
		f(r)
		return r
	}
	if !credentialMatches(mk(func(r *http.Request) {
		r.Header.Set("ApiKey", key)
	}), key) {
		t.Error("ApiKey header should match")
	}
	if !credentialMatches(mk(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
	}), key) {
		t.Error("Bearer should match")
	}
	// Query param: required because <video src> can't set headers.
	if !credentialMatches(httptest.NewRequest("GET", "/pornhub/stream/x?apikey="+key, nil), key) {
		t.Error("apikey query param should match")
	}
	if credentialMatches(mk(func(r *http.Request) {
		r.Header.Set("ApiKey", "wrong")
	}), key) {
		t.Error("wrong key must not match")
	}
	if credentialMatches(httptest.NewRequest("GET", "/config", nil), key) {
		t.Error("no credential must not match")
	}
}
