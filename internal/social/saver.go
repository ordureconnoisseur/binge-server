// Package social implements "save a social post into Stash": download
// the media through binge-server's egress, place it under the configured
// library root as <root>/<source>/<handle>/, scan it into Stash, and
// write the metadata (source Studio + Tag + performer + url + date +
// caption).
//
// Everything is config-driven so it ships as a product: the write root
// (where this daemon writes) and the Stash root (the path Stash sees the
// same files at) are both runtime config; Studios/Tags are resolved by
// NAME (auto-created), never hardcoded ids. When the paths aren't
// configured the feature is simply unavailable (the API returns 503 and
// the client hides the button).
package social

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/pornhub"
	"github.com/ordureconnoisseur/binge-server/internal/stash"
)

// ErrNotConfigured — the social write/stash roots aren't set.
var ErrNotConfigured = errors.New("social save not configured (set the library paths)")

// sourceLabels maps an internal source key to its Studio/Tag display
// name. Generic defaults (not user-specific), safe to ship.
var sourceLabels = map[string]string{
	"x":         "X",
	"twitter":   "X",
	"reddit":    "Reddit",
	"redgifs":   "Redgifs",
	"instagram": "Instagram",
	"pornhub":   "PornHub",
}

const socialParentTag = "Social Media"

type Saver struct {
	stash   *stash.Client
	http    *http.Client
	pornhub *pornhub.Client
	logger  *slog.Logger

	mu        sync.RWMutex
	writeRoot string // path THIS daemon writes to (e.g. /library/social)
	stashRoot string // path Stash sees the same files at (e.g. Z:\Media\social)
}

func New(st *stash.Client, ph *pornhub.Client, logger *slog.Logger) *Saver {
	return &Saver{
		stash:   st,
		pornhub: ph,
		http: &http.Client{
			Timeout: 180 * time.Second,
			// Every hop is re-checked, not just the first. Validating
			// only the URL handed in left an open redirect on any
			// allowed host (out.reddit.com, x.com/i/redirect) as a way
			// to walk the fetch onto the LAN: the response body is then
			// written into the library and scanned into Stash, so the
			// caller reads it back. Every sibling proxy in this project
			// already guards its redirects; this client and the Stash
			// one were the two that did not.
			CheckRedirect: checkMediaRedirect,
		},
		logger: logger,
	}
}

func (s *Saver) log(msg, detail string) {
	if s.logger != nil {
		s.logger.Warn(msg, "detail", detail)
	}
}

// SetPaths updates the write/stash roots (from env seed or POST /config).
func (s *Saver) SetPaths(writeRoot, stashRoot string) {
	s.mu.Lock()
	s.writeRoot = strings.TrimRight(writeRoot, `/\`)
	s.stashRoot = strings.TrimRight(stashRoot, `/\`)
	s.mu.Unlock()
}

func (s *Saver) roots() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeRoot, s.stashRoot
}

// Configured reports whether both roots are set (gates the feature).
func (s *Saver) Configured() bool {
	w, st := s.roots()
	return w != "" && st != ""
}

type SaveRequest struct {
	PerformerStashID string `json:"performerStashId"`
	Source           string `json:"source"` // x | reddit | redgifs | instagram
	Handle           string `json:"handle"` // optional; derived if empty
	ID               string `json:"id"`     // post/tweet id (used for the filename)
	MediaURL         string `json:"mediaUrl"`
	Kind             string `json:"kind"` // image | video
	SourceURL        string `json:"sourceUrl"`
	Text             string `json:"text"`
	CreatedUtc       int64  `json:"createdUtc"`
}

type SaveResult struct {
	StashType string `json:"stashType"` // scene | image
	Path      string `json:"path"`
	Handle    string `json:"handle"`
	// Pending = file saved + scan triggered; metadata is being applied in
	// the background (lands once Stash finishes scanning).
	Pending bool `json:"pending"`
}

func (s *Saver) Save(ctx context.Context, req SaveRequest) (*SaveResult, error) {
	writeRoot, stashRoot := s.roots()
	if writeRoot == "" || stashRoot == "" {
		return nil, ErrNotConfigured
	}
	label := sourceLabels[strings.ToLower(req.Source)]
	if label == "" {
		return nil, fmt.Errorf("unknown source %q", req.Source)
	}
	if req.MediaURL == "" {
		return nil, errors.New("missing mediaUrl")
	}

	// Resolve the handle for the folder: explicit > from performer urls[]
	// > performer name. Best-effort performer fetch (also used for meta).
	perf, _ := s.stash.FetchPerformer(ctx, req.PerformerStashID)
	handle := req.Handle
	if handle == "" {
		handle = stash.PerformerHandle(perf, strings.ToLower(req.Source))
	}
	if handle == "" {
		handle = perf.Name
	}
	handle = stash.SanitizeSegment(handle)

	ext := extFromURL(req.MediaURL, req.Kind)
	base := stash.SanitizeSegment(req.ID)
	if base == "unknown" {
		base = stash.SanitizeSegment(strings.TrimSuffix(path.Base(urlPath(req.MediaURL)), "."+ext))
	}
	filename := base + "." + ext

	// `src` is caller-supplied and reaches filepath.Join, which CLEANS a
	// path but does not CONFINE it: Join("/lib/social", "../../etc", h)
	// resolves to /etc/h. handle and base were already sanitised; this one
	// was missed, which made the write location caller-chosen.
	src := stash.SanitizeSegment(strings.ToLower(req.Source))
	writeDir := filepath.Join(writeRoot, src, handle)
	writePath := filepath.Join(writeDir, filename)

	// Belt-and-braces: whatever the segments were, refuse to write outside
	// the configured root. Cheap, and it catches the next missed sanitise
	// rather than relying on every future call site remembering.
	if !underRoot(writeRoot, writePath) {
		return nil, fmt.Errorf(
			"refusing to write outside the library root: %s", writePath)
	}

	// Build the Stash-side path with the separator implied by stashRoot
	// (Windows Stash = backslash, Linux = forward slash).
	sep := "/"
	if strings.Contains(stashRoot, `\`) {
		sep = `\`
	}
	stashDir := strings.Join([]string{stashRoot, src, handle}, sep)
	stashPath := stashDir + sep + filename

	// Download + scan happen up front (so failures surface to the caller),
	// but the file may not register in Stash for a while when the scan
	// queue is backed up. So tagging runs in the background and the caller
	// gets an immediate "queued" result — the file is on disk + scanning,
	// and the metadata lands when the scan completes.
	if err := s.download(ctx, req, writeDir, writePath); err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if err := s.stash.MetadataScan(ctx, []string{stashDir}); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	stashType := "image"
	if req.Kind == "video" {
		stashType = "scene"
	}
	go s.tagWhenScanned(req, label, base, stashType, stashPath, handle)

	return &SaveResult{StashType: stashType, Path: stashPath, Handle: handle, Pending: true}, nil
}

// tagWhenScanned polls (detached from the request) until the scan has
// registered the placed file, then writes the source studio/tag +
// performer/url/date/caption. Best-effort: the file is already saved, so
// a failure here just leaves an untagged item.
func (s *Saver) tagWhenScanned(req SaveRequest, label, token, stashType, stashPath, handle string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var stashID string
	for stashID == "" {
		var id string
		if stashType == "scene" {
			id, _ = s.stash.FindSceneIDByPath(ctx, token, stashPath)
		} else {
			id, _ = s.stash.FindImageIDByPath(ctx, token, stashPath)
		}
		if id != "" {
			stashID = id
			break
		}
		select {
		case <-ctx.Done():
			s.log("save tag timed out (file saved, untagged)", stashPath)
			return
		case <-time.After(3 * time.Second):
		}
	}

	studioID, err := s.stash.EnsureStudio(ctx, label)
	if err != nil {
		s.log("ensure studio failed", err.Error())
		return
	}
	tagID, err := s.stash.EnsureTag(ctx, label, socialParentTag)
	if err != nil {
		s.log("ensure tag failed", err.Error())
		return
	}
	meta := stash.EntityMeta{
		PerformerID: req.PerformerStashID,
		StudioID:    studioID,
		TagID:       tagID,
		URL:         req.SourceURL,
		Date:        dateFromUnix(req.CreatedUtc),
		Details:     req.Text,
	}
	if stashType == "scene" {
		err = s.stash.UpdateSceneMeta(ctx, stashID, meta)
	} else {
		err = s.stash.UpdateImageMeta(ctx, stashID, meta)
	}
	if err != nil {
		s.log("save metadata write failed", err.Error())
	}
}

func (s *Saver) download(ctx context.Context, req SaveRequest, dir, dest string) error {
	// A file being present is not proof that a download finished.
	//
	// yt-dlp writes straight to the final name, and its parent context
	// is the request's four-minute deadline, so a long video is killed
	// mid-write and leaves a truncated file exactly where a complete
	// one would be. This check then read that as success on the next
	// attempt: the scan ran, the video was marked saved, and the
	// half-file was accepted as the real thing for good. An empty file
	// is always wrong, and a video that came in under a megabyte is
	// wrong often enough to be worth re-fetching.
	if fi, err := os.Stat(dest); err == nil {
		minSize := int64(1)
		if req.Kind == "video" {
			minSize = 1 << 20
		}
		if fi.Size() >= minSize {
			return nil // already downloaded
		}
		s.log("replacing an implausibly small existing file", dest)
		if err := os.Remove(dest); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// req.MediaURL is caller-supplied and flows into a yt-dlp argv
	// (pornhub) or an http GET (everything else). Both are dangerous with
	// an unvalidated string: a value like "--config-location=..." is read
	// by yt-dlp as an option rather than a URL, and an arbitrary host is
	// an SSRF sink. Require a real http(s) URL to a host the source is
	// expected to serve, before either path sees it.
	if err := validateMediaURL(req.Source, req.MediaURL); err != nil {
		return err
	}
	// PornHub has no direct media URL — yt-dlp extracts + downloads the
	// video from the watch page (req.MediaURL).
	if strings.ToLower(req.Source) == "pornhub" {
		if s.pornhub == nil {
			return errors.New("pornhub downloader unavailable")
		}
		return s.pornhub.Download(ctx, req.MediaURL, dest)
	}
	// The source travels with the request so every redirect can be
	// checked against the same host list the original URL was.
	hr, err := http.NewRequestWithContext(
		context.WithValue(ctx, mediaSourceKey, req.Source),
		"GET", req.MediaURL, nil,
	)
	if err != nil {
		return err
	}
	hr.Header.Set("User-Agent", "Mozilla/5.0 (compatible; binge-server)")
	// Some CDNs gate on Referer (same reason the reddit/redgifs proxies do).
	switch strings.ToLower(req.Source) {
	case "redgifs":
		hr.Header.Set("Referer", "https://www.redgifs.com/")
	case "reddit":
		hr.Header.Set("Referer", "https://www.reddit.com/")
	}
	resp, err := s.http.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// Cap the copy. Without it a single small /save call can write an
	// unbounded response into the library and fill the volume. The limit
	// is generous for the images and short clips this path handles;
	// full-length pornhub video goes through yt-dlp, not here.
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxSaveBytes+1))
	if err == nil && n > maxSaveBytes {
		err = fmt.Errorf("media exceeds %d byte limit", maxSaveBytes)
	}
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, dest)
}

// maxSaveBytes caps a single non-video Save download. Images and short
// clips sit far under this; it exists only to stop an unbounded write.
const maxSaveBytes = 512 << 20 // 512 MiB

// checkMediaRedirect keeps a media download inside the host set of the
// source it started from. The source is carried on the request context
// because http.Client gives the hook no other way to know it.
func checkMediaRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	source, _ := req.Context().Value(mediaSourceKey).(string)
	if source == "" {
		return fmt.Errorf("redirect with no source to check against")
	}
	return validateMediaURL(source, req.URL.String())
}

type mediaSourceCtxKey struct{}

// mediaSourceKey carries the save source through to the redirect hook.
var mediaSourceKey = mediaSourceCtxKey{}

// mediaHosts lists the hostnames each source is allowed to fetch from.
// A save request naming a source may only pull media from that source's
// own CDNs, which stops the download path being pointed at internal
// addresses (169.254.169.254, the local Stash, other LAN services) and
// stops a hostile value masquerading as a URL.
var mediaHosts = map[string][]string{
	"pornhub":   {"pornhub.com", "phncdn.com"},
	"redgifs":   {"redgifs.com"},
	"reddit":    {"redd.it", "redditmedia.com", "reddit.com"},
	"x":         {"twimg.com", "twitter.com", "x.com"},
	"twitter":   {"twimg.com", "twitter.com", "x.com"},
	"instagram": {"cdninstagram.com", "fbcdn.net"},
}

// validateMediaURL rejects anything that is not an http(s) URL whose host
// belongs to the named source. Host match is exact or a dotted-suffix, so
// "evilpornhub.com" does not pass for "pornhub". A leading "-" cannot
// survive url.Parse as a host, but the scheme and host checks reject it
// regardless.
func validateMediaURL(source, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("bad media url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("media url must be http(s)")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("media url has no host")
	}
	allowed, ok := mediaHosts[strings.ToLower(source)]
	if !ok {
		return fmt.Errorf("unknown save source %q", source)
	}
	for _, h := range allowed {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("media host %q not allowed for source %q", host, source)
}

// mediaExts is what a saved file is allowed to be called.
//
// An allowlist rather than a shape check, because the guesses come from
// a URL and both sources produce confident nonsense. A PornHub save is
// requested with the watch page URL, whose path ends ".php", so every
// PornHub save landed as <viewkey>.php: yt-dlp wrote an mp4, Stash
// would not index a .php file as a scene, the video was marked saved
// and so vanished from the feed, and all the user was left with was an
// orphan in their library. redgifs ".m4s" fails the same way. And the
// ?format= branch is a query parameter under a remote host's control,
// unsanitised, concatenated into a filename: it escaped the performer
// directory, which only the write-root confinement stopped.
var mediaExts = map[string]bool{
	"mp4": true, "m4v": true, "webm": true, "mov": true, "mkv": true,
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true,
	"avif": true, "heic": true,
}

// extFromURL guesses the file extension from the media URL (path ext or
// a ?format= param), falling back by kind. Anything not recognisably a
// media extension falls back rather than being trusted.
func extFromURL(raw, kind string) string {
	if e := strings.TrimPrefix(strings.ToLower(path.Ext(urlPath(raw))), "."); mediaExts[e] {
		return e
	}
	if u, err := url.Parse(raw); err == nil {
		f := strings.ToLower(strings.TrimPrefix(u.Query().Get("format"), "."))
		if mediaExts[f] {
			return f
		}
	}
	if kind == "video" {
		return "mp4"
	}
	return "jpg"
}

func urlPath(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return raw
}

func dateFromUnix(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}

// underRoot reports whether path is inside root after both are cleaned and
// made absolute. Uses filepath.Rel rather than a string prefix so that
// "/libraryX" is not treated as inside "/library".
func underRoot(root, path string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
