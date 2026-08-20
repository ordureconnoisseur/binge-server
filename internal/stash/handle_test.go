package stash

import "testing"

// This value picks the folder a save is written into, so a URL that
// merely contains a platform's name must not name the folder. The
// poller was fixed to parse the host rather than search the string;
// this is the copy the save path uses, and it was not.
func TestPerformerHandle(t *testing.T) {
	h := func(source string, urls ...string) string {
		return PerformerHandle(Performer{URLs: urls}, source)
	}

	// Real profile URLs still resolve.
	if got := h("reddit", "https://www.reddit.com/user/alice"); got != "alice" {
		t.Fatalf("reddit user = %q", got)
	}
	if got := h("reddit", "reddit.com/u/alice"); got != "alice" {
		t.Fatalf("scheme-less reddit = %q", got)
	}
	if got := h("x", "https://x.com/alice"); got != "alice" {
		t.Fatalf("x = %q", got)
	}
	if got := h("x", "https://twitter.com/alice/status/1"); got != "alice" {
		t.Fatalf("twitter with a path = %q", got)
	}
	if got := h("redgifs", "https://www.redgifs.com/users/alice"); got != "alice" {
		t.Fatalf("redgifs = %q", got)
	}
	if got := h("instagram", "https://instagram.com/alice/"); got != "alice" {
		t.Fatalf("instagram = %q", got)
	}

	// A host that merely contains the platform's name must not match.
	for _, bad := range []string{
		"https://elsewhere.example/reddit.com/user/victim",
		"https://notreddit.com/user/victim",
		"https://reddit.com.evil.example/user/victim",
		"https://example.com/?ref=reddit.com/user/victim",
	} {
		if got := h("reddit", bad); got != "" {
			t.Fatalf("reddit matched %q -> %q", bad, got)
		}
	}
	for _, bad := range []string{
		"https://elsewhere.example/x.com/victim",
		"https://indexxx.com/m/victim",
		"https://pornbox.com/application/model/victim",
	} {
		if got := h("x", bad); got != "" {
			t.Fatalf("x matched %q -> %q", bad, got)
		}
	}

	// An unknown source asks for nothing.
	if got := h("nowhere", "https://x.com/alice"); got != "" {
		t.Fatalf("unknown source returned %q", got)
	}
}

// This value names the folder a save is written into, so a site's own
// routing must not end up as a directory called after it.
func TestReservedSegmentsAreNotHandles(t *testing.T) {
	h := func(source string, url string) string {
		return PerformerHandle(Performer{URLs: []string{url}}, source)
	}
	for _, c := range []struct{ source, url string }{
		{"x", "https://x.com/i/status/1234567890"},
		{"x", "https://x.com/home"},
		{"x", "https://x.com/search?q=alice"},
		{"x", "https://twitter.com/intent/user?screen_name=alice"},
		{"instagram", "https://instagram.com/p/ABC123/"},
		{"instagram", "https://instagram.com/explore/tags/x/"},
		{"reddit", "https://reddit.com/u/../escape"},
	} {
		if got := h(c.source, c.url); got != "" {
			t.Fatalf("%s %q named a folder %q", c.source, c.url, got)
		}
	}
	// A real handle in the same shape still resolves.
	if got := h("x", "https://x.com/alice"); got != "alice" {
		t.Fatalf("real handle = %q", got)
	}
	// Non-ASCII is kept as itself rather than percent-escaped.
	if got := h("instagram", "https://instagram.com/josé"); got != "josé" {
		t.Fatalf("non-ascii handle = %q", got)
	}
}
