// Package pornhub pulls a performer's PornHub videos via yt-dlp. Listing
// + per-video metadata feed the cached pornhub_videos table; stream URLs
// are extracted on demand (they're time/IP-locked, so they can't be
// cached and must be proxied from the same egress that extracted them).
//
// PornHub 410s non-browser requests, so every yt-dlp call uses browser
// impersonation (--impersonate, backed by curl_cffi in the image).
package pornhub

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const impersonate = "chrome"

// Progressive (http) mp4 format preference — avoids HLS so we can proxy a
// single byte stream without ffmpeg/segment rewriting. PornHub exposes
// these as format ids "240p".."1080p".
const progressiveFormat = "720p/480p/1080p/240p/best[protocol^=http][vcodec!=none]"

type Client struct {
	bin     string
	timeout time.Duration
}

func New() *Client {
	return &Client{bin: "yt-dlp", timeout: 90 * time.Second}
}

// Host-anchored, as in twitter/client.go: an unanchored match picks the
// name out of any URL that merely contains the string.
var reModel = regexp.MustCompile(`(?i)(?:^|[./])pornhub\.com/(pornstar|model)/([^/?#]+)`)

// ModelURLFromURLs returns the canonical <.../videos> page for the first
// pornhub.com pornstar/model link in a performer's urls[], or "".
func ModelURLFromURLs(urls []string) string {
	for _, u := range urls {
		if m := reModel.FindStringSubmatch(u); len(m) == 3 {
			return fmt.Sprintf("https://www.pornhub.com/%s/%s/videos",
				strings.ToLower(m[1]), m[2])
		}
	}
	return ""
}

var reViewKey = regexp.MustCompile(`viewkey=([A-Za-z0-9]+)`)

func viewKey(url string) string {
	if m := reViewKey.FindStringSubmatch(url); len(m) == 2 {
		return m[1]
	}
	return ""
}

// VideoRef is the cheap flat-playlist shape (no metadata).
type VideoRef struct {
	ViewKey string
	URL     string
	Title   string
}

// ListVideos returns up to `limit` of a model's most-recent videos
// (cheap: one flat-playlist call, title + url only).
func (c *Client) ListVideos(ctx context.Context, modelURL string, limit int) ([]VideoRef, error) {
	if limit <= 0 {
		limit = 30
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.run(ctx,
		"--impersonate", impersonate,
		"--flat-playlist", "-J",
		"--playlist-items", "1-"+itoa(limit),
		"--", modelURL,
	)
	if err != nil {
		return nil, err
	}
	var pl struct {
		Entries []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
			ID    string `json:"id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out, &pl); err != nil {
		return nil, fmt.Errorf("parse playlist: %w", err)
	}
	refs := make([]VideoRef, 0, len(pl.Entries))
	for _, e := range pl.Entries {
		vk := e.ID
		if vk == "" {
			vk = viewKey(e.URL)
		}
		if vk == "" || e.URL == "" {
			continue
		}
		refs = append(refs, VideoRef{ViewKey: vk, URL: e.URL, Title: e.Title})
	}
	return refs, nil
}

// VideoMeta is the stable per-video metadata cached in the DB.
type VideoMeta struct {
	ViewKey    string
	Title      string
	ThumbURL   string
	Duration   int // seconds
	ViewCount  int64
	UploadDate string // YYYYMMDD
	CreatedUtc int64
}

// FetchVideoMeta pulls one video's metadata (thumbnail/duration/views/
// date). Heavier than ListVideos — used only for videos not yet cached.
func (c *Client) FetchVideoMeta(ctx context.Context, videoURL string) (*VideoMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.run(ctx,
		"--impersonate", impersonate,
		"--no-download", "-j",
		"--", videoURL,
	)
	if err != nil {
		return nil, err
	}
	var d struct {
		ID         string  `json:"id"`
		Title      string  `json:"title"`
		Thumbnail  string  `json:"thumbnail"`
		Duration   float64 `json:"duration"`
		ViewCount  int64   `json:"view_count"`
		UploadDate string  `json:"upload_date"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("parse video json: %w", err)
	}
	m := &VideoMeta{
		ViewKey:    d.ID,
		Title:      d.Title,
		ThumbURL:   d.Thumbnail,
		Duration:   int(d.Duration),
		ViewCount:  d.ViewCount,
		UploadDate: d.UploadDate,
		CreatedUtc: dateToUnix(d.UploadDate),
	}
	if m.ViewKey == "" {
		m.ViewKey = viewKey(videoURL)
	}
	return m, nil
}

// ExtractStreamURL resolves a fresh progressive-mp4 URL for inline
// proxying. The URL is time/IP-locked, so callers must proxy it
// immediately from this same egress.
func (c *Client) ExtractStreamURL(ctx context.Context, videoURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.run(ctx,
		"--impersonate", impersonate,
		"-f", progressiveFormat, "-g", "--", videoURL,
	)
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(out))
	if i := strings.IndexByte(url, '\n'); i >= 0 {
		url = url[:i]
	}
	if !strings.HasPrefix(url, "http") {
		return "", fmt.Errorf("no stream url resolved")
	}
	return url, nil
}

// Download fetches the full video to destPath (progressive mp4).
func (c *Client) Download(ctx context.Context, videoURL, destPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, err := c.run(ctx,
		"--impersonate", impersonate,
		"-f", progressiveFormat,
		"-o", destPath,
		"--no-part",
		// Never resume. yt-dlp defaults to --continue, so a file
		// already at the target is APPENDED to from its current size.
		// A leftover from a download that was killed, or from a second
		// save of the same item running at the same time, was then
		// completed by the next attempt into a full-size file whose
		// opening bytes belong to the abandoned one. That renamed into
		// the library, was marked saved, and the existing-file
		// short-circuit meant it was never fetched again: permanent,
		// silent corruption of a file the user believes is fine.
		"--no-continue",
		"--", videoURL,
	)
	return err
}

// scrapeScript fetches a model page with browser impersonation
// (curl_cffi — Go can't fetch PornHub directly, it 410s) and returns a
// {viewkey: mediabook-preview-url} JSON map parsed from the video tiles.
const scrapeScript = `
import sys, json, re
from curl_cffi import requests
r = requests.get(sys.argv[1], impersonate="chrome", timeout=25)
out = {}
for t in re.findall(r'<li[^>]*videoblock[^>]*>.*?</li>', r.text, re.S):
    vk = re.search(r'viewkey=([A-Za-z0-9]+)', t)
    mb = re.search(r'data-mediabook="([^"]+)"', t)
    if vk and mb:
        out[vk.group(1)] = mb.group(1)
print(json.dumps(out))
`

// FetchPreviews scrapes a model page for its videos' hover-preview
// (mediabook) webm URLs, keyed by viewkey. Time-locked URLs, so callers
// fetch fresh + proxy.
func (c *Client) FetchPreviews(ctx context.Context, modelURL string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", scrapeScript, modelURL)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("preview scrape: %s", msg)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(stdout.String()), &m); err != nil {
		return nil, fmt.Errorf("parse previews: %w", err)
	}
	return m, nil
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	// Retry transient rate-limits (403/429) — they happen when the
	// backfill poll and an on-demand stream hit PornHub from the same exit
	// IP at once. A short backoff almost always clears them.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		out, err := c.runOnce(ctx, args...)
		if err == nil {
			return out, nil
		}
		lastErr = err
		m := err.Error()
		if !strings.Contains(m, "403") && !strings.Contains(m, "429") &&
			!strings.Contains(m, "Forbidden") && !strings.Contains(m, "rate") {
			return nil, err // non-transient — don't retry
		}
	}
	return nil, lastErr
}

func (c *Client) runOnce(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		// Keep just the last error line — yt-dlp is chatty.
		if lines := strings.Split(msg, "\n"); len(lines) > 0 {
			msg = lines[len(lines)-1]
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("yt-dlp: %s", msg)
	}
	return []byte(stdout.String()), nil
}

func dateToUnix(yyyymmdd string) int64 {
	if len(yyyymmdd) != 8 {
		return 0
	}
	t, err := time.Parse("20060102", yyyymmdd)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
