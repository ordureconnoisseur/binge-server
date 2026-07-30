// Package twitter pulls a performer's X (Twitter) media by shelling out
// to gallery-dl, authenticated with the user's own session cookies
// (auth_token + ct0). X's official API is dead for self-service, so we
// drive the same unofficial web GraphQL endpoints the browser uses —
// gallery-dl handles the query-id churn, cursor pagination, and video
// variant selection, and emits one JSON record per media file.
//
// Fetches are on-demand (per performer, when the binge client opens
// their feed) rather than a bulk poll: that keeps the request volume
// low and human-paced, and avoids hammering X across hundreds of
// handles. Results are short-lived and cached by the caller.
//
// Egress note: in production this runs inside binge-server's container,
// which shares the tailscale bypass netns → Mullvad NL exit. X serves
// fine through that datacenter IP (validated 2026-06-26).
package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNoCookies — the X auth_token/ct0 pair hasn't been configured.
	ErrNoCookies = errors.New("x cookies not configured")
	// ErrBadHandle — handle failed validation (would be unsafe to embed
	// in the fetch URL).
	ErrBadHandle = errors.New("invalid x handle")
)

// handleRe is X's username rule: 1–15 chars, letters/digits/underscore.
// Validating before we build the URL keeps an attacker-controlled Stash
// URL field from injecting shell/URL nonsense into the gallery-dl arg.
var handleRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// reXHandle pulls a handle out of a twitter.com / x.com profile URL.
// Skips reserved path segments that aren't real profiles.
//
// The leading (?:^|[./]) is load-bearing: "x.com" is a suffix of plenty of
// unrelated hosts, and without a host boundary this matched inside them.
// indexxx.com/m/<name> became @m and pornbox.com/application/model/<id>
// became @application — 119 of 1087 performers in one real library, most
// of which then fetched some stranger's media instead of nothing.
var reXHandle = regexp.MustCompile(`(?i)(?:^|[./])(?:twitter|x)\.com/([A-Za-z0-9_]{1,15})(?:[/?#]|$)`)

var xReserved = map[string]bool{
	"home": true, "search": true, "explore": true, "notifications": true,
	"messages": true, "i": true, "intent": true, "share": true, "hashtag": true,
	"settings": true, "compose": true,
}

// HandleFromURLs returns the first valid X handle found in a performer's
// urls[] (twitter.com or x.com), or "" if none.
func HandleFromURLs(urls []string) string {
	for _, u := range urls {
		m := reXHandle.FindStringSubmatch(u)
		if len(m) == 2 {
			h := m[1]
			if !xReserved[strings.ToLower(h)] {
				return h
			}
		}
	}
	return ""
}

// Client shells out to gallery-dl. Cookies are written to a gallery-dl
// config file on SetCookies; FetchMedia points gallery-dl at it.
type Client struct {
	mu         sync.RWMutex
	configPath string
	bin        string
	hasCookies bool
	curAuth    string // last-applied cookies, to skip redundant rewrites
	curCT0     string
	timeout    time.Duration
}

// New returns a client that writes its gallery-dl config under dir.
// bin is the gallery-dl executable (just "gallery-dl" on PATH).
func New(dir, bin string) *Client {
	if bin == "" {
		bin = "gallery-dl"
	}
	return &Client{
		configPath: filepath.Join(dir, "gdl-x.json"),
		bin:        bin,
		timeout:    45 * time.Second,
	}
}

// gdlConfig is the minimal gallery-dl config we emit — twitter cookies
// plus a couple of behaviour toggles. videos:true makes gallery-dl pick
// the best mp4 variant; cards/text-tweets off keeps it to actual media.
type gdlConfig struct {
	Extractor struct {
		Twitter struct {
			Cookies    map[string]string `json:"cookies"`
			Videos     bool              `json:"videos"`
			TextTweets bool              `json:"text-tweets"`
			Retweets   bool              `json:"retweets"`
		} `json:"twitter"`
	} `json:"extractor"`
}

// SetCookies writes (or clears) the gallery-dl config from the stored
// auth_token + ct0. Both empty → cookies considered unset and FetchMedia
// will refuse. Safe to call repeatedly; only rewrites on change.
func (c *Client) SetCookies(authToken, ct0 string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if authToken == "" || ct0 == "" {
		c.hasCookies = false
		return nil
	}
	// Unchanged since last apply — config file already on disk.
	if c.hasCookies && authToken == c.curAuth && ct0 == c.curCT0 {
		return nil
	}
	var cfg gdlConfig
	cfg.Extractor.Twitter.Cookies = map[string]string{"auth_token": authToken, "ct0": ct0}
	cfg.Extractor.Twitter.Videos = true
	cfg.Extractor.Twitter.TextTweets = false
	cfg.Extractor.Twitter.Retweets = false
	buf, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.configPath, buf, 0o600); err != nil {
		return fmt.Errorf("write gallery-dl config: %w", err)
	}
	c.hasCookies = true
	c.curAuth = authToken
	c.curCT0 = ct0
	return nil
}

// HasCookies reports whether usable cookies are configured.
func (c *Client) HasCookies() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hasCookies
}

// Media is one media file from a performer's X media tab, normalized to
// the same camelCase shape the rest of binge-server's API uses.
type Media struct {
	TweetID       string `json:"tweetId"`
	TweetURL      string `json:"tweetUrl"`
	Kind          string `json:"kind"` // "image" | "video"
	MediaURL      string `json:"mediaUrl"`
	Text          string `json:"text,omitempty"`
	AuthorHandle  string `json:"authorHandle,omitempty"`
	AuthorNick    string `json:"authorNick,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	Sensitive     bool   `json:"sensitive"`
	FavoriteCount int    `json:"favoriteCount,omitempty"`
	ViewCount     int64  `json:"viewCount,omitempty"`
	CreatedUtc    int64  `json:"createdUtc"`
}

// xMeta is the slice of gallery-dl's per-file metadata we keep.
type xMeta struct {
	TweetID       int64  `json:"tweet_id"`
	Date          string `json:"date"` // "2006-01-02 15:04:05" (UTC)
	Content       string `json:"content"`
	Extension     string `json:"extension"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Sensitive     bool   `json:"sensitive"`
	FavoriteCount int    `json:"favorite_count"`
	ViewCount     int64  `json:"view_count"`
	Author        struct {
		Name string `json:"name"` // the @handle
		Nick string `json:"nick"` // display name
	} `json:"author"`
}

// FetchMedia returns up to count media files from x.com/<handle>/media,
// newest first. count caps the gallery-dl --range so we fetch only as
// deep as the caller scrolls.
func (c *Client) FetchMedia(ctx context.Context, handle string, count int) ([]Media, error) {
	if !c.HasCookies() {
		return nil, ErrNoCookies
	}
	if !handleRe.MatchString(handle) {
		return nil, ErrBadHandle
	}
	if count <= 0 || count > 200 {
		count = 40
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	c.mu.RLock()
	cfgPath, bin := c.configPath, c.bin
	c.mu.RUnlock()

	cmd := exec.CommandContext(ctx, bin,
		"-c", cfgPath,
		"--no-download",
		"-j",
		"--range", "1-"+strconv.Itoa(count),
		"https://x.com/"+handle+"/media",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gallery-dl: %s", msg)
	}

	return parseGalleryDL([]byte(stdout.String()), handle)
}

// parseGalleryDL turns gallery-dl's -j message stream into []Media. The
// stream is a JSON array of tuples; [3, url, meta] is a media file,
// other tuple types (directory/tweet headers) are skipped.
func parseGalleryDL(out []byte, handle string) ([]Media, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return []Media{}, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, fmt.Errorf("decode gallery-dl output: %w", err)
	}

	media := make([]Media, 0, len(entries))
	for _, e := range entries {
		var parts []json.RawMessage
		if err := json.Unmarshal(e, &parts); err != nil || len(parts) < 3 {
			continue
		}
		var typ int
		if err := json.Unmarshal(parts[0], &typ); err != nil || typ != 3 {
			continue
		}
		var url string
		if err := json.Unmarshal(parts[1], &url); err != nil || url == "" {
			continue
		}
		var m xMeta
		_ = json.Unmarshal(parts[2], &m)

		item := Media{
			MediaURL:      url,
			Text:          m.Content,
			Width:         m.Width,
			Height:        m.Height,
			Sensitive:     m.Sensitive,
			FavoriteCount: m.FavoriteCount,
			ViewCount:     m.ViewCount,
			AuthorHandle:  m.Author.Name,
			AuthorNick:    m.Author.Nick,
			Kind:          classify(url, m.Extension),
		}
		if m.TweetID > 0 {
			item.TweetID = strconv.FormatInt(m.TweetID, 10)
			h := m.Author.Name
			if h == "" {
				h = handle
			}
			item.TweetURL = "https://x.com/" + h + "/status/" + item.TweetID
		}
		if t, err := time.Parse("2006-01-02 15:04:05", m.Date); err == nil {
			item.CreatedUtc = t.Unix()
		}
		media = append(media, item)
	}
	return media, nil
}

func classify(url, ext string) string {
	if ext == "mp4" || strings.Contains(url, "video.twimg.com") || strings.Contains(url, "/amplify_video/") {
		return "video"
	}
	return "image"
}
