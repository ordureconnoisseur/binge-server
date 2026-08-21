package twitter

import "testing"

// A handle decides whose media tab the daemon drives with the user's own
// X session, so it has to come from the host and not from anywhere a
// stranger's URL can put the characters "x.com/".

func TestHandleFromURLsIgnoresTheHandleInSomeoneElsesPath(t *testing.T) {
	cases := []string{
		"https://evil.example/x.com/victim",
		"https://www.indexxx.com/m/x.com/victim",
		"https://example.org/redirect?to=/twitter.com/victim",
		"https://example.com/foo/twitter.com/someoneelse/bar",
		"https://evilx.com/victim",
		"https://x.com.evil.example/victim",
	}
	for _, in := range cases {
		if got := HandleFromURLs([]string{in}); got != "" {
			t.Errorf("HandleFromURLs(%q) = %q, want no handle", in, got)
		}
	}
}

func TestHandleFromURLsStillReadsRealProfiles(t *testing.T) {
	cases := map[string]string{
		"https://x.com/alice":            "alice",
		"https://twitter.com/alice":      "alice",
		"https://www.x.com/alice":        "alice",
		"x.com/alice":                    "alice",
		"https://x.com/alice/media":      "alice",
		"https://x.com/alice?lang=en":    "alice",
		"https://mobile.twitter.com/bob": "bob",
	}
	for in, want := range cases {
		if got := HandleFromURLs([]string{in}); got != want {
			t.Errorf("HandleFromURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleFromURLsSkipsReservedSegments(t *testing.T) {
	for _, in := range []string{
		"https://x.com/home",
		"https://x.com/i/status/1",
		"https://twitter.com/search?q=x",
	} {
		if got := HandleFromURLs([]string{in}); got != "" {
			t.Errorf("HandleFromURLs(%q) = %q, want no handle", in, got)
		}
	}
}
