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
