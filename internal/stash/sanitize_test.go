package stash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A segment has to survive the round trip to disk unchanged. Anything
// that comes back different breaks the tag pass, which finds the saved
// file by comparing the path string it built against the path Stash
// recorded.

func TestSanitizeSegmentKeepsValidUTF8(t *testing.T) {
	// 40 CJK characters is 120 bytes. Slicing at byte 80 lands in the
	// middle of the 27th character.
	got := SanitizeSegment(strings.Repeat("あ", 40))
	if !utf8.ValidString(got) {
		t.Fatalf("segment is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 40 {
		t.Errorf("kept %d characters of 40, want all of them", n)
	}
}

func TestSanitizeSegmentSurvivesTheFilesystem(t *testing.T) {
	// The failure this protects against is silent: the file is created,
	// stat succeeds, and only the name Stash reports back differs.
	dir := t.TempDir()
	name := SanitizeSegment(strings.Repeat("あ", 40)) + ".mp4"
	want := filepath.Join(dir, name)

	f, err := os.Create(want)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.Close()

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("expected one file, got %d", len(ents))
	}
	if ents[0].Name() != name {
		t.Errorf("asked the filesystem for %q, it stored %q",
			name, ents[0].Name())
	}
}

func TestSanitizeSegmentDoesNotMergeTwoHandles(t *testing.T) {
	// Two different people must not truncate onto the same folder.
	long := strings.Repeat("あ", 40)
	if a, b := SanitizeSegment(long+"alice"), SanitizeSegment(long+"bob"); a == b {
		t.Errorf("two handles collapsed to the same segment %q", a)
	}
}

func TestSanitizeSegmentStripsControlCharacters(t *testing.T) {
	// These reach os.Create as "invalid argument", which surfaces as a
	// failed save with nothing explaining it.
	got := SanitizeSegment("bad\x00name\nhere")
	if strings.ContainsFunc(got, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		t.Fatalf("control characters survived: %q", got)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), got+".mp4"))
	if err != nil {
		t.Fatalf("sanitised segment still rejected by the filesystem: %v", err)
	}
	f.Close()
}

func TestSanitizeSegmentNeverEndsInDotOrSpace(t *testing.T) {
	// Windows drops a trailing dot or space when it stores the name, so
	// one exposed by the truncation would desync the two sides again.
	got := SanitizeSegment(strings.Repeat("a", 79) + ". trailing")
	if strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
		t.Errorf("segment ends in a character Windows will strip: %q", got)
	}
}

func TestSanitizeSegmentRespectsBothLimits(t *testing.T) {
	// A character can be four bytes, so a cap counted only in characters
	// still produces a name ext4 refuses with "file name too long".
	for _, in := range []string{
		strings.Repeat("a", 300),
		strings.Repeat("あ", 300),
		strings.Repeat("😀", 300),
	} {
		got := SanitizeSegment(in)
		if !utf8.ValidString(got) {
			t.Errorf("not valid UTF-8 for %d-byte input", len(in))
		}
		if len(got) > maxSegmentBytes+4 {
			t.Errorf("kept %d bytes, over the %d-byte limit",
				len(got), maxSegmentBytes)
		}
		if utf8.RuneCountInString(got) > maxSegmentRunes {
			t.Errorf("kept %d characters, over the %d limit",
				utf8.RuneCountInString(got), maxSegmentRunes)
		}
		f, err := os.Create(filepath.Join(t.TempDir(), got+".mp4"))
		if err != nil {
			t.Errorf("filesystem refused the sanitised name: %v", err)
			continue
		}
		f.Close()
	}
}
