package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// Client uses Reddit's public `.json` listing endpoints, authenticated
// via a browser session cookie copied from a logged-in tab. This
// sidesteps Reddit's developer-app gate (killed for self-service in
// Nov 2025) while still respecting the user's NSFW prefs — without
// a valid cookie, NSFW user/sub feeds return empty.
//
// `cookie` is the literal value of the HTTP Cookie header — either
// just `reddit_session=<value>` or a full multi-cookie string. The
// user copies it from DevTools when configuring binge-server.
//
// Reddit ToS still requires a distinctive User-Agent — set via
// REDDIT_USER_AGENT in main.go.
type Client struct {
	mu        sync.RWMutex
	cookie    string
	userAgent string
	http      *http.Client
}

func New(cookie, userAgent string) *Client {
	return &Client{
		cookie:    cookie,
		userAgent: userAgent,
		http: &http.Client{
			Timeout: 30 * time.Second,
			// Reddit redirects unauthenticated NSFW requests to
			// /quarantine /over18 interstitials. We never want to
			// follow those — let the caller see the redirect status
			// and treat it as forbidden.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetCookie swaps the session cookie at runtime. The poller calls this
// at the top of every tick after reading the current value from the
// config store, so cookie rotation via the binge UI takes effect on
// the next poll cycle without a daemon restart.
func (c *Client) SetCookie(cookie string) {
	c.mu.Lock()
	c.cookie = cookie
	c.mu.Unlock()
}

func (c *Client) currentCookie() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cookie
}

// ErrSuspended — listing returned a suspended/notfound condition.
// Poller marks the performer and stops scheduling them.
var (
	ErrSuspended    = errors.New("reddit handle suspended")
	ErrNotFound     = errors.New("reddit handle not found")
	ErrForbidden    = errors.New("reddit handle forbidden")
	ErrRateLimit    = errors.New("reddit rate-limited")
	ErrCookieExpired = errors.New("reddit session cookie invalid or expired")
)

// Post mirrors the slice of Reddit's listing JSON we use.
type Post struct {
	ID         string  `json:"id"`        // base36 (no t3_ prefix)
	Name       string  `json:"name"`      // full name "t3_<id>"
	Subreddit  string  `json:"subreddit"`
	Author     string  `json:"author"`
	Title      string  `json:"title"`
	Selftext   string  `json:"selftext"`
	IsSelf     bool    `json:"is_self"`
	IsVideo    bool    `json:"is_video"`
	IsGallery  bool    `json:"is_gallery"`
	Over18     bool    `json:"over_18"`
	URL        string  `json:"url"`
	Domain     string  `json:"domain"`
	Permalink  string  `json:"permalink"` // starts with "/r/..."
	PostHint   string  `json:"post_hint"`
	CreatedUTC float64 `json:"created_utc"`
	Thumbnail  string  `json:"thumbnail"` // can be "self"/"default"/"nsfw"/url
	Media      struct {
		RedditVideo struct {
			FallbackURL string `json:"fallback_url"`
		} `json:"reddit_video"`
	} `json:"media"`
	Preview struct {
		Images []struct {
			Source struct {
				URL string `json:"url"`
			} `json:"source"`
		} `json:"images"`
	} `json:"preview"`
	// Gallery shape: items in order, each pointing at a key in
	// MediaMetadata. The image URL is in the metadata's `s.u` field
	// (source size); `p` is an array of progressively smaller previews.
	GalleryData struct {
		Items []struct {
			MediaID string `json:"media_id"`
		} `json:"items"`
	} `json:"gallery_data"`
	MediaMetadata map[string]struct {
		Status string `json:"status"`
		E      string `json:"e"` // "Image" / "AnimatedImage" / etc.
		M      string `json:"m"` // mime
		S      struct {
			U   string `json:"u"`   // image url (raw_json=1 = unescaped)
			GIF string `json:"gif"` // present for AnimatedImage
			MP4 string `json:"mp4"` // present for AnimatedImage
		} `json:"s"`
	} `json:"media_metadata"`
}

type listing struct {
	Data struct {
		Children []struct {
			Kind string `json:"kind"`
			Data Post   `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// FetchUserSubmissions returns the latest posts from a Reddit user
// account. `handle` is the username, no /u/ prefix.
func (c *Client) FetchUserSubmissions(ctx context.Context, handle string, limit int) ([]Post, error) {
	return c.fetchListing(ctx,
		fmt.Sprintf("https://www.reddit.com/user/%s/submitted.json?limit=%d&sort=new&raw_json=1",
			url.PathEscape(handle), limit))
}

// FetchSubNew returns the latest posts from a subreddit. `handle` is
// the sub name, no /r/ prefix.
func (c *Client) FetchSubNew(ctx context.Context, handle string, limit int) ([]Post, error) {
	return c.fetchListing(ctx,
		fmt.Sprintf("https://www.reddit.com/r/%s/new.json?limit=%d&raw_json=1",
			url.PathEscape(handle), limit))
}

func (c *Client) fetchListing(ctx context.Context, endpoint string) ([]Post, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	if cookie := c.currentCookie(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		// fall through
	case 301, 302, 303, 307, 308:
		// Reddit redirects unauthenticated NSFW requests to an
		// over18 interstitial. With our CheckRedirect we see the
		// redirect status directly — treat as cookie-expired so the
		// user sees a clear error in logs.
		return nil, ErrCookieExpired
	case 401:
		return nil, ErrCookieExpired
	case 403:
		return nil, ErrForbidden
	case 404:
		return nil, ErrNotFound
	case 429:
		return nil, ErrRateLimit
	default:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MB cap
		return nil, fmt.Errorf("reddit %d: %s", resp.StatusCode, raw)
	}

	var l listing
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return nil, fmt.Errorf("listing decode: %w", err)
	}

	out := make([]Post, 0, len(l.Data.Children))
	for _, c := range l.Data.Children {
		if c.Kind == "t3" {
			out = append(out, c.Data)
		}
	}
	return out, nil
}

// Classification produced from a raw Post. media_url for redgifs is
// left empty here — the poller calls the redgifs resolver and fills
// it in before persisting.
type Class struct {
	Kind          string // image | video | text | link
	MediaURL      string
	LinkURL       string
	ThumbURL      string
	Domain        string
	NeedsRedgifs  bool
	RedgifsSlug   string
}

// imageExts matches what Reddit serves directly as i.redd.it images.
var imageExts = []string{".jpg", ".jpeg", ".png", ".gif", ".webp"}

func hasImageExt(u string) bool {
	lower := strings.ToLower(u)
	for _, e := range imageExts {
		if strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}

func isRedgifsDomain(d string) bool {
	d = strings.ToLower(d)
	return d == "redgifs.com" || d == "www.redgifs.com" || d == "v3.redgifs.com"
}

// extractRedgifsSlug parses the slug from a redgifs URL.
// Examples that need to work:
//   https://www.redgifs.com/watch/abcdefgxyz123       -> "abcdefgxyz123"
//   https://www.redgifs.com/ifr/abcdefgxyz123          -> "abcdefgxyz123"
//   https://redgifs.com/watch/abcdefgxyz123?queryjunk  -> "abcdefgxyz123"
func extractRedgifsSlug(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	// Slug is always the last segment.
	slug := parts[len(parts)-1]
	// Drop any extension just in case ("xyz123.mp4" → "xyz123").
	slug = strings.TrimSuffix(slug, path.Ext(slug))
	return strings.ToLower(slug)
}

// Reddit JSON-encodes preview URLs with `&amp;` escapes. Unescape so
// the URL is usable as-is.
func unescapeURL(u string) string {
	return html.UnescapeString(u)
}

func firstPreviewURL(p Post) string {
	for _, img := range p.Preview.Images {
		if img.Source.URL != "" {
			return unescapeURL(img.Source.URL)
		}
	}
	return ""
}

// Classify converts a raw Post to its persistence-ready shape. The
// returned media_url is empty for redgifs videos — caller resolves via
// the redgifs client.
func Classify(p Post) Class {
	c := Class{Domain: p.Domain, ThumbURL: firstPreviewURL(p)}
	if p.IsSelf {
		c.Kind = "text"
		return c
	}
	// Gallery posts (multi-image) — first item's source image becomes
	// the media; the rest are reachable via the "open on reddit" CTA.
	// Detect BEFORE other branches because a gallery post can also
	// have post_hint="image" set, which would mis-route to the URL
	// path (which is a reddit.com/gallery/<id>, not an image).
	if p.IsGallery && len(p.GalleryData.Items) > 0 {
		first := p.GalleryData.Items[0].MediaID
		md, ok := p.MediaMetadata[first]
		if ok && md.Status == "valid" {
			// AnimatedImage galleries are rare but exist; prefer mp4
			// over gif/static.
			if md.E == "AnimatedImage" && md.S.MP4 != "" {
				c.Kind = "video"
				c.MediaURL = md.S.MP4
				return c
			}
			if md.S.U != "" {
				c.Kind = "image"
				c.MediaURL = md.S.U
				return c
			}
		}
		// Fallback: gallery with empty/invalid metadata — link card.
		c.Kind = "link"
		c.LinkURL = p.URL
		return c
	}
	// Redgifs check BEFORE v.redd.it: redgifs cross-posts can show up
	// with `is_video=true` and a v.redd.it fallback URL, but we always
	// prefer the redgifs direct mp4 (has audio, no DASH muxing).
	if isRedgifsDomain(p.Domain) {
		c.Kind = "video"
		c.NeedsRedgifs = true
		c.RedgifsSlug = extractRedgifsSlug(p.URL)
		return c
	}
	if p.IsVideo && p.Media.RedditVideo.FallbackURL != "" {
		// v.redd.it splits audio into a separate DASH stream — the
		// fallback_url is video-only, so inline playback is silent.
		// Render as a link card instead; CTA opens reddit's native
		// player which handles the DASH audio track.
		c.Kind = "link"
		c.LinkURL = p.URL
		return c
	}
	if p.Domain == "i.redd.it" || p.PostHint == "image" || hasImageExt(p.URL) {
		c.Kind = "image"
		c.MediaURL = p.URL
		return c
	}
	c.Kind = "link"
	c.LinkURL = p.URL
	return c
}

// PermalinkURL turns p.Permalink ("/r/.../comments/...") into a full
// reddit.com URL the frontend can open.
func PermalinkURL(p Post) string {
	if p.Permalink == "" {
		return ""
	}
	if strings.HasPrefix(p.Permalink, "http") {
		return p.Permalink
	}
	return "https://www.reddit.com" + p.Permalink
}
