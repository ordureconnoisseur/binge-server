package twitter

import "testing"

func TestHandleFromURLs(t *testing.T) {
	cases := []struct {
		name string
		urls []string
		want string
	}{
		{"x.com", []string{"https://x.com/keniamusicr"}, "keniamusicr"},
		{"twitter.com", []string{"https://twitter.com/AngelaWhite"}, "AngelaWhite"},
		{"www subdomain", []string{"https://www.twitter.com/foo"}, "foo"},
		{"trailing path", []string{"https://x.com/foo/media"}, "foo"},
		{"query string", []string{"https://x.com/foo?lang=en"}, "foo"},
		{"no scheme", []string{"x.com/foo"}, "foo"},
		{"reserved segment skipped", []string{"https://x.com/i/status/1", "https://x.com/real"}, "real"},
		{"none", []string{"https://instagram.com/foo"}, ""},

		// Hosts that merely END in "x.com". These are the regression: each
		// used to yield a bogus handle, and @m in particular is a real
		// account, so the feed showed a stranger's posts.
		{"indexxx", []string{"https://www.indexxx.com/m/kenia-music"}, ""},
		{"pornbox", []string{"https://pornbox.com/application/model/253381"}, ""},
		{"netflix", []string{"https://www.netflix.com/title/80100172"}, ""},

		// The real ordering bug: a lookalike host sorted before the genuine
		// profile meant the genuine one was never reached.
		{"lookalike first, real second", []string{
			"https://pornbox.com/application/model/253381",
			"https://www.indexxx.com/m/kenia-music",
			"https://x.com/keniamusicr",
		}, "keniamusicr"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HandleFromURLs(tc.urls); got != tc.want {
				t.Errorf("HandleFromURLs(%q) = %q, want %q", tc.urls, got, tc.want)
			}
		})
	}
}
