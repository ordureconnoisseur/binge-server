package social

import "testing"

// The extension decides the filename, and Stash decides what to index
// by extension. Guessing it from a URL produces confident nonsense.
func TestExtFromURL(t *testing.T) {
	cases := []struct {
		name, raw, kind, want string
	}{
		{
			// What the client actually sends for a PornHub save. The
			// path ends .php, so every save landed as <viewkey>.php:
			// yt-dlp wrote an mp4, Stash never indexed it, and the
			// video was marked saved and vanished from the feed.
			name: "a pornhub watch page is not a php file",
			raw:  "https://www.pornhub.com/view_video.php?viewkey=ph5f35abc",
			kind: "video", want: "mp4",
		},
		{
			name: "a redgifs fragment is not m4s either",
			raw:  "https://media.redgifs.com/SomeSlug.m4s",
			kind: "video", want: "mp4",
		},
		{
			// A query parameter under a remote host's control, spliced
			// into a filename. It escaped the performer directory.
			name: "a format parameter cannot carry a path",
			raw:  "https://pbs.twimg.com/media/AB?format=.%2F..%2F..%2Fpwned.jpg",
			kind: "image", want: "jpg",
		},
		{
			name: "nor an executable name",
			raw:  "https://pbs.twimg.com/media/AB?format=exe",
			kind: "image", want: "jpg",
		},
		{
			name: "a real format parameter is honoured",
			raw:  "https://pbs.twimg.com/media/AB?format=png&name=orig",
			kind: "image", want: "png",
		},
		{
			name: "a leading dot on the parameter is tolerated",
			raw:  "https://pbs.twimg.com/media/AB?format=.webp",
			kind: "image", want: "webp",
		},
		{
			name: "an ordinary path extension is honoured",
			raw:  "https://i.redd.it/abc123.png",
			kind: "image", want: "png",
		},
		{
			name: "uppercase is normalised",
			raw:  "https://i.redd.it/abc123.JPEG",
			kind: "image", want: "jpeg",
		},
		{
			name: "an unparseable url still yields something sane",
			raw:  "::not a url::",
			kind: "video", want: "mp4",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extFromURL(c.raw, c.kind); got != c.want {
				t.Fatalf("extFromURL(%q, %q) = %q, want %q", c.raw, c.kind, got, c.want)
			}
		})
	}
}
