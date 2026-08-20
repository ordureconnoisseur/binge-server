package poller

import "testing"

// A performer's urls[] is free text from the user's own library, and
// these patterns decide which Reddit account the daemon polls on their
// behalf. Without a host boundary they match inside any URL that
// happens to contain the string, which is how the sibling pattern in
// twitter/client.go mis-attributed 119 performers in a real library
// before it was anchored. These two were missed at the time.
func TestRedditHandlePatterns(t *testing.T) {
	user := func(s string) string { return redditPathHandle(s, reRedditUserPath) }
	sub := func(s string) string { return redditPathHandle(s, reRedditSubPath) }

	// Real profile URLs still resolve.
	for _, in := range []string{
		"https://reddit.com/user/alice",
		"https://www.reddit.com/user/alice",
		"https://old.reddit.com/u/alice",
		"http://www.reddit.com/user/alice/",
		"REDDIT.COM/USER/alice",
	} {
		if got := user(in); got != "alice" {
			t.Fatalf("user(%q) = %q, want alice", in, got)
		}
	}
	if got := sub("https://www.reddit.com/r/pics"); got != "pics" {
		t.Fatalf("sub = %q, want pics", got)
	}

	// A host that merely contains the string must not match.
	for _, in := range []string{
		"https://elsewhere.example/reddit.com/user/victim",
		"https://notreddit.com/user/victim",
		"https://example.com/?ref=reddit.com/user/victim",
		"https://myreddit.com/r/victim",
		"https://reddit.com.evil.example/user/victim",
		"https://evil.example/?u=https://reddit.com/user/victim",
	} {
		if got := user(in); got != "" {
			t.Fatalf("user(%q) = %q, want no match", in, got)
		}
		if got := sub(in); got != "" {
			t.Fatalf("sub(%q) = %q, want no match", in, got)
		}
	}
}
