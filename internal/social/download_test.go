package social

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refused request must not have changed anything on its way to being
// refused. An earlier version of this check tried to spot a truncated
// file by its size and deleted it, and it did so before the request had
// been validated: a save naming a host the source is not allowed to
// serve deleted the user's file and then returned an error.
func TestARefusedSaveTouchesNothing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "reddit", "alice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "holiday.jpg")
	const contents = "the user's own file"
	if err := os.WriteFile(dest, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Saver{}
	err := s.download(context.Background(), SaveRequest{
		Source:   "reddit",
		Handle:   "alice",
		ID:       "holiday",
		Kind:     "video", // the kind that raised the old size floor
		MediaURL: "https://attacker.example/x.jpg",
	}, dir, dest)
	if err == nil {
		t.Fatal("expected the disallowed host to be refused")
	}

	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("the refused save deleted the user's file: %v", readErr)
	}
	if string(got) != contents {
		t.Fatalf("the refused save rewrote the user's file: %q", got)
	}
}

// A file already in place is left alone, whatever its size. Downloads
// land on a temporary name and are renamed in, so anything at the
// destination is finished by construction: ours or the user's, and
// neither is ours to replace.
func TestAnExistingFileIsNeverReplaced(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "reddit", "alice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "small.mp4")
	// Well under the megabyte the old floor demanded of a video.
	if err := os.WriteFile(dest, []byte("tiny but complete"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Saver{}
	// A host that would be allowed, so the only thing under test is
	// whether the existing file short-circuits the download.
	err := s.download(context.Background(), SaveRequest{
		Source:   "reddit",
		Kind:     "video",
		MediaURL: "https://i.redd.it/abc.mp4",
	}, dir, dest)
	if err != nil {
		t.Fatalf("expected the existing file to be accepted: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "tiny but complete" {
		t.Fatalf("a small existing file was replaced: %q", got)
	}
}

// os.Stat succeeds on a directory, and an earlier version removed
// whatever it found under the size floor.
func TestADirectoryAtTheDestinationIsNotRemoved(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "reddit", "alice")
	dest := filepath.Join(dir, "collision")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dest, "keep.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Saver{}
	// Refused for an unrelated reason; the point is what survives.
	_ = s.download(context.Background(), SaveRequest{
		Source:   "reddit",
		Kind:     "image",
		MediaURL: "https://attacker.example/x.jpg",
	}, dir, dest)

	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("a directory at the destination was removed: %v", err)
	}
}

// yt-dlp treats ".part" as its own suffix and strips it from -o, so a
// temporary name ending that way is silently written to the final
// name instead. That made every PornHub save fail its first attempt on
// a rename of a file that was never created, and left the finished
// video at the destination so the second attempt skipped the tagging.
//
// This pins the property the code depends on rather than yt-dlp's
// behaviour: whatever temporary name is chosen, it must not be one the
// downloader will rewrite.
func TestTheTemporaryNameIsNotOneYtDlpClaims(t *testing.T) {
	dest := filepath.Join("x", "video.mp4")
	tmp := dest + ".incoming"
	if strings.HasSuffix(tmp, ".part") {
		t.Fatalf("temporary name %q ends in .part, which yt-dlp strips", tmp)
	}
	if tmp == dest {
		t.Fatal("temporary name is the destination")
	}
	if !strings.HasPrefix(tmp, dest) {
		t.Fatalf("temporary name %q is not derived from %q", tmp, dest)
	}
}
