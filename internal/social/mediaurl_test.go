package social

import "testing"

// req.MediaURL flows into a yt-dlp argv and an http GET, so a value that
// is not a real URL to the source's own CDN is either an argument
// injection or an SSRF. validateMediaURL is the gate.
func TestValidateMediaURL(t *testing.T) {
	ok := []struct{ source, url string }{
		{"pornhub", "https://www.pornhub.com/view_video.php?viewkey=abc"},
		{"pornhub", "https://dl.phncdn.com/videos/x.mp4"},
		{"redgifs", "https://media.redgifs.com/Clip.mp4"},
		{"reddit", "https://i.redd.it/abc.jpg"},
		{"x", "https://pbs.twimg.com/media/x.jpg"},
	}
	for _, c := range ok {
		if err := validateMediaURL(c.source, c.url); err != nil {
			t.Errorf("validateMediaURL(%q, %q) = %v, want nil", c.source, c.url, err)
		}
	}

	bad := []struct {
		name, source, url string
	}{
		// The argument-injection payload: read by yt-dlp as an option.
		{"config-location option", "pornhub", "--config-location=/tmp/evil.conf"},
		{"bare option", "pornhub", "--version"},
		// SSRF sinks.
		{"metadata endpoint", "reddit", "http://169.254.169.254/latest/meta-data/"},
		{"local stash", "reddit", "http://127.0.0.1:9999/graphql"},
		{"file scheme", "reddit", "file:///etc/passwd"},
		// Lookalike host must not pass a dotted-suffix check.
		{"lookalike host", "pornhub", "https://evilpornhub.com/x.mp4"},
		{"suffix-not-dotted", "pornhub", "https://notphncdn.com/x.mp4"},
		// Wrong source for a real CDN.
		{"redgifs url under pornhub", "pornhub", "https://media.redgifs.com/x.mp4"},
		// Unknown source.
		{"unknown source", "myspace", "https://media.redgifs.com/x.mp4"},
		{"empty", "pornhub", ""},
	}
	for _, c := range bad {
		if err := validateMediaURL(c.source, c.url); err == nil {
			t.Errorf("%s: validateMediaURL(%q, %q) = nil, want rejection", c.name, c.source, c.url)
		}
	}
}
