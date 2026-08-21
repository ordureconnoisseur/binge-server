package social

import "testing"

// The path built here is the one handed to Stash for the scan and then
// used to find the scene again afterwards. If Stash cleans it into a
// different string, the file is indexed but the tag pass never matches
// it, and the save stays untagged with nothing reporting a failure.

func TestStashLibraryPathHasNoDoubledSeparator(t *testing.T) {
	cases := []struct {
		name string
		root string
		want string
	}{
		{
			name: "windows root with a trailing backslash",
			root: `Z:\Media\social\`,
			want: `Z:\Media\social\reddit\alice\p1.mp4`,
		},
		{
			name: "windows root without one",
			root: `Z:\Media\social`,
			want: `Z:\Media\social\reddit\alice\p1.mp4`,
		},
		{
			name: "posix root with a trailing slash",
			root: "/data/media/social/",
			want: "/data/media/social/reddit/alice/p1.mp4",
		},
		{
			name: "posix root without one",
			root: "/data/media/social",
			want: "/data/media/social/reddit/alice/p1.mp4",
		},
		{
			name: "drive root",
			root: `Z:\`,
			want: `Z:\reddit\alice\p1.mp4`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got, err := stashLibraryPath(c.root, "reddit", "alice", "p1.mp4")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestStashLibraryPathRefusesABareSeparator(t *testing.T) {
	// Joining onto `\` produced `\\reddit\alice`, which Windows reads as
	// a UNC path: the scan would be aimed at a network host named after
	// the source rather than anywhere in the library.
	for _, root := range []string{`\`, "/", `\\`, "//"} {
		if _, got, err := stashLibraryPath(root, "reddit", "alice", "p1.mp4"); err == nil {
			t.Errorf("root %q was accepted and built %q", root, got)
		}
	}
}
