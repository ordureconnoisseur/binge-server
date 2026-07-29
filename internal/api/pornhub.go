package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// ── stream-URL extraction cache ─────────────────────────────────────
// Extracted PornHub mp4 URLs are time/IP-locked, so we cache them only
// briefly (long enough to serve a viewing session's range requests, well
// under their validfrom window) and re-extract after.
const phStreamTTL = 4 * time.Minute

type phStreamEntry struct {
	url string
	at  time.Time
}
type phStreamCache struct {
	mu sync.Mutex
	m  map[string]phStreamEntry
}

func newPHStreamCache() *phStreamCache { return &phStreamCache{m: map[string]phStreamEntry{}} }
func (c *phStreamCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || time.Since(e.at) > phStreamTTL {
		return "", false
	}
	return e.url, true
}
func (c *phStreamCache) put(id, url string) {
	c.mu.Lock()
	c.m[id] = phStreamEntry{url: url, at: time.Now()}
	c.mu.Unlock()
}

// ── preview (mediabook) cache ───────────────────────────────────────
// Per-model scrape result (viewkey -> preview webm url). One scrape
// serves all of a performer's video previews; refreshed every few minutes
// because the URLs are time-locked.
const phPreviewTTL = 5 * time.Minute

type phPreviewEntry struct {
	m  map[string]string
	at time.Time
}
type phPreviewCache struct {
	mu sync.Mutex
	m  map[string]phPreviewEntry // keyed by model url
}

func newPHPreviewCache() *phPreviewCache { return &phPreviewCache{m: map[string]phPreviewEntry{}} }
func (c *phPreviewCache) get(modelURL string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[modelURL]
	if !ok || time.Since(e.at) > phPreviewTTL {
		return nil, false
	}
	return e.m, true
}
func (c *phPreviewCache) put(modelURL string, m map[string]string) {
	c.mu.Lock()
	c.m[modelURL] = phPreviewEntry{m: m, at: time.Now()}
	c.mu.Unlock()
}

// ── owned-viewkey dedup cache ───────────────────────────────────────
// The set of PornHub viewkeys already in the user's Stash library
// (scenes with a pornhub.com url). Feed/stories exclude these so owned
// content doesn't resurface as "new". Refreshed lazily; the Stash sweep
// is a few paginated queries, cheap enough to block a feed request when
// stale but cached so repeat opens are instant.
const phOwnedTTL = 5 * time.Minute

type phOwnedCache struct {
	mu  sync.Mutex
	set map[string]struct{}
	at  time.Time
}

func newPHOwnedCache() *phOwnedCache { return &phOwnedCache{} }

// ownedViewkeys returns the cached owned-viewkey set, refreshing from
// Stash when stale. Best-effort: a Stash error keeps the previous set
// (or an empty one) rather than breaking the feed — dedup degrades to
// "show everything," never to an error.
func (s *Server) ownedViewkeys(ctx context.Context) map[string]struct{} {
	s.phOwned.mu.Lock()
	if s.phOwned.set != nil && time.Since(s.phOwned.at) < phOwnedTTL {
		set := s.phOwned.set
		s.phOwned.mu.Unlock()
		return set
	}
	s.phOwned.mu.Unlock()

	set, err := s.stash.OwnedPornhubViewkeys(ctx)
	if err != nil {
		s.log.Warn("pornhub owned-viewkey refresh", "err", err)
		s.phOwned.mu.Lock()
		old := s.phOwned.set
		s.phOwned.mu.Unlock()
		if old != nil {
			return old
		}
		return map[string]struct{}{}
	}
	s.phOwned.mu.Lock()
	s.phOwned.set = set
	s.phOwned.at = time.Now()
	s.phOwned.mu.Unlock()
	return set
}

// dropOwned filters out videos whose viewkey is already owned in Stash.
func dropOwned(vids []phVideo, owned map[string]struct{}) []phVideo {
	if len(owned) == 0 {
		return vids
	}
	out := make([]phVideo, 0, len(vids))
	for _, v := range vids {
		if _, ok := owned[v.ID]; ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// ── feed ────────────────────────────────────────────────────────────

type phVideo struct {
	ID         string  `json:"id"`         // viewkey
	Title      *string `json:"title"`
	SourceURL  string  `json:"sourceUrl"`  // pornhub watch page
	ThumbURL   *string `json:"thumbUrl"`   // raw phncdn (web proxies it)
	Duration   int     `json:"duration"`
	ViewCount  int64   `json:"viewCount"`
	UploadDate *string `json:"uploadDate"`
	CreatedUtc int64   `json:"createdUtc"`
}

func (s *Server) scanVideos(rows *sql.Rows) ([]phVideo, error) {
	out := []phVideo{}
	for rows.Next() {
		var v phVideo
		var title, thumb, up sql.NullString
		if err := rows.Scan(&v.ID, &title, &v.SourceURL, &thumb, &v.Duration, &v.ViewCount, &up, &v.CreatedUtc); err != nil {
			return nil, err
		}
		v.Title = nullStr(title)
		v.ThumbURL = nullStr(thumb)
		v.UploadDate = nullStr(up)
		out = append(out, v)
	}
	return out, rows.Err()
}

// GET /pornhub/feed/{stashId} — the performer's cached PornHub videos.
func (s *Server) pornhubFeed(w http.ResponseWriter, r *http.Request) {
	if _, err := strconv.Atoi(chi.URLParam(r, "stashId")); err != nil {
		http.Error(w, "bad stashId", http.StatusBadRequest)
		return
	}
	limit := 60
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	// saved_at IS NULL drops videos saved through binge; dropOwned then
	// removes any already in the Stash library (owned some other way).
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT video_id, title, url, thumb_url, duration, view_count, upload_date, created_utc
		FROM pornhub_videos WHERE performer_stash_id=? AND saved_at IS NULL
		ORDER BY created_utc DESC LIMIT ?`,
		chi.URLParam(r, "stashId"), limit)
	if err != nil {
		s.log.Error("pornhubFeed query", "err", err)
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	vids, err := s.scanVideos(rows)
	if err != nil {
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, dropOwned(vids, s.ownedViewkeys(r.Context())))
}

// ── stories ─────────────────────────────────────────────────────────

type phStoryDigest struct {
	PerformerStashID   int       `json:"performerStashId"`
	PerformerName      string    `json:"performerName"`
	PerformerImagePath string    `json:"performerImagePath"`
	PerformerFavorite  bool      `json:"performerFavorite"`
	LatestCreatedUtc   int64     `json:"latestCreatedUtc"`
	VideoCount         int       `json:"videoCount"`
	Videos             []phVideo `json:"videos"`
}

func (s *Server) pornhubStories(w http.ResponseWriter, r *http.Request) {
	sinceUtc, _ := strconv.ParseInt(r.URL.Query().Get("sinceUtc"), 10, 64)
	owned := s.ownedViewkeys(r.Context())
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT p.stash_id, p.name, p.image_path, p.favorite,
		       MAX(v.created_utc), COUNT(*)
		FROM pornhub_performers p JOIN pornhub_videos v ON v.performer_stash_id=p.stash_id
		WHERE v.created_utc >= ? AND v.saved_at IS NULL
		GROUP BY p.stash_id, p.name, p.image_path, p.favorite
		ORDER BY MAX(v.created_utc) DESC`, sinceUtc)
	if err != nil {
		s.log.Error("pornhubStories digest", "err", err)
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	digs := []phStoryDigest{}
	for rows.Next() {
		var d phStoryDigest
		var fav int
		if err := rows.Scan(&d.PerformerStashID, &d.PerformerName, &d.PerformerImagePath, &fav, &d.LatestCreatedUtc, &d.VideoCount); err != nil {
			rows.Close()
			http.Error(w, "db", http.StatusInternalServerError)
			return
		}
		d.PerformerFavorite = fav != 0
		digs = append(digs, d)
	}
	rows.Close()
	out := make([]phStoryDigest, 0, len(digs))
	for i := range digs {
		vr, err := s.db.QueryContext(r.Context(), `
			SELECT video_id, title, url, thumb_url, duration, view_count, upload_date, created_utc
			FROM pornhub_videos WHERE performer_stash_id=? AND created_utc >= ? AND saved_at IS NULL
			ORDER BY created_utc DESC LIMIT 25`, digs[i].PerformerStashID, sinceUtc)
		if err != nil {
			continue
		}
		vids, _ := s.scanVideos(vr)
		vr.Close()
		vids = dropOwned(vids, owned)
		// A performer whose recent videos are all owned/saved drops out
		// of the row entirely; recompute count + latest from what's left
		// (videos are DESC, so the head is the latest).
		if len(vids) == 0 {
			continue
		}
		digs[i].Videos = vids
		digs[i].VideoCount = len(vids)
		digs[i].LatestCreatedUtc = vids[0].CreatedUtc
		out = append(out, digs[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// ── stream proxy ────────────────────────────────────────────────────
// Extract the progressive mp4 (cached briefly) then relay its bytes with
// Range support, so the client plays inline without the IP/time-locked
// CDN URL ever leaving this egress.
var phStreamHTTP = &http.Client{Timeout: 60 * time.Second}

func (s *Server) pornhubStream(w http.ResponseWriter, r *http.Request) {
	if s.pornhub == nil {
		http.Error(w, "pornhub disabled", http.StatusServiceUnavailable)
		return
	}
	vid := chi.URLParam(r, "videoId")
	mp4, ok := s.phStreams.get(vid)
	if !ok {
		// Resolve the watch URL from the cache table, then extract.
		var watch string
		if err := s.db.QueryRowContext(r.Context(), `SELECT url FROM pornhub_videos WHERE video_id=?`, vid).Scan(&watch); err != nil {
			http.Error(w, "unknown video", http.StatusNotFound)
			return
		}
		u, err := s.pornhub.ExtractStreamURL(r.Context(), watch)
		if err != nil {
			s.log.Warn("pornhub stream extract", "video", vid, "err", err)
			http.Error(w, "extract failed", http.StatusBadGateway)
			return
		}
		mp4 = u
		s.phStreams.put(vid, u)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, mp4, nil)
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; binge-server)")
	req.Header.Set("Referer", "https://www.pornhub.com/")
	if rh := r.Header.Get("Range"); rh != "" {
		req.Header.Set("Range", rh)
	}
	resp, err := phStreamHTTP.Do(req)
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// ── thumbnail proxy ─────────────────────────────────────────────────
// PornHub thumbnails are on phncdn.com (an adult CDN some networks
// block); relay them through this egress. Allowlist phncdn only.
var phThumbHTTP = &http.Client{Timeout: 20 * time.Second}

func (s *Server) pornhubThumb(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	// Leading dot matters: without it "myevilphncdn.com" — a
	// registrable domain — satisfies the suffix check.
	host := strings.ToLower(u.Hostname())
	if err != nil || u.Scheme != "https" ||
		!(host == "phncdn.com" || strings.HasSuffix(host, ".phncdn.com")) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "GET", raw, nil)
	req.Header.Set("Referer", "https://www.pornhub.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; binge-server)")
	resp, err := phThumbHTTP.Do(req)
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// GET /pornhub/preview/{videoId} — proxy the hover-preview (mediabook)
// webm so the client can loop it in story/feed cards (like Stash
// previews). The url is scraped fresh per model (cached ~5m) + time/IP-
// locked, so it must be proxied from this egress.
func (s *Server) pornhubPreview(w http.ResponseWriter, r *http.Request) {
	if s.pornhub == nil {
		http.Error(w, "pornhub disabled", http.StatusServiceUnavailable)
		return
	}
	vid := chi.URLParam(r, "videoId")
	var modelURL string
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT pp.ph_url FROM pornhub_videos pv
		JOIN pornhub_performers pp ON pv.performer_stash_id = pp.stash_id
		WHERE pv.video_id = ?`, vid).Scan(&modelURL); err != nil {
		http.Error(w, "unknown video", http.StatusNotFound)
		return
	}
	previews, ok := s.phPreviews.get(modelURL)
	if !ok {
		m, err := s.pornhub.FetchPreviews(r.Context(), modelURL)
		if err != nil {
			s.log.Warn("pornhub preview scrape", "video", vid, "err", err)
			http.Error(w, "preview unavailable", http.StatusBadGateway)
			return
		}
		s.phPreviews.put(modelURL, m)
		previews = m
	}
	previewURL, ok := previews[vid]
	if !ok {
		http.Error(w, "no preview", http.StatusNotFound)
		return
	}
	if u, err := url.Parse(previewURL); err != nil || !strings.HasSuffix(strings.ToLower(u.Host), "phncdn.com") {
		http.Error(w, "bad preview url", http.StatusBadGateway)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "GET", previewURL, nil)
	req.Header.Set("Referer", "https://www.pornhub.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; binge-server)")
	if rh := r.Header.Get("Range"); rh != "" {
		req.Header.Set("Range", rh)
	}
	resp, err := phStreamHTTP.Do(req)
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
