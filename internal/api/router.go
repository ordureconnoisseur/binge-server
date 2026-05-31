package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/ordureconnoisseur/binge-server/internal/configstore"
)

// Poller is the slim interface the API needs — a synchronous PollAll.
// The router debounces and dispatches it to a goroutine on /refresh.
type Poller interface {
	PollAll(ctx context.Context) error
}

type Server struct {
	db     *sql.DB
	store  *configstore.Store
	poller Poller
	log    *slog.Logger

	refreshMu     sync.Mutex
	lastRefreshAt time.Time

	allowedOrigins []string // CORS allowlist (parsed); loopback always OK
}

// refreshCooldown — minimum time between /reddit/refresh-triggered
// polls. Manual hammering won't translate to bursts on Reddit.
const refreshCooldown = 30 * time.Second

func New(db *sql.DB, store *configstore.Store, poller Poller, log *slog.Logger, allowedOrigin string) *Server {
	return &Server{
		db:             db,
		store:          store,
		poller:         poller,
		log:            log,
		allowedOrigins: parseOrigins(allowedOrigin),
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.corsMiddleware)
	r.Get("/healthz", s.healthz)
	r.Get("/config", s.getConfig)
	r.Post("/config", s.postConfig)
	r.Get("/reddit/stories", s.redditStories)
	r.Get("/reddit/feed/{stashId}", s.redditFeed)
	r.Post("/reddit/refresh", s.redditRefresh)
	r.Get("/redgifs/proxy", s.proxyRedgifs)
	r.Head("/redgifs/proxy", s.proxyRedgifs)
	r.Get("/reddit/proxy", s.proxyReddit)
	r.Head("/reddit/proxy", s.proxyReddit)
	return r
}

// ── /redgifs/proxy ────────────────────────────────────────────────────
//
// Streams a media.redgifs.com / v3.redgifs.com asset through this
// daemon so the browser doesn't have to talk to redgifs directly.
// Reasons we proxy:
//   - redgifs 403s requests whose Referer isn't their own origin (we
//     can't override `referrerpolicy` on a <video> in all browsers).
//   - The user's PC is on a UK university network that may block
//     adult-content CDNs; binge-server already egresses via Mullvad NL.
//
// The query parameter `url` must be a full https URL whose host ends
// in `.redgifs.com` — anything else is rejected so this can't double
// as an open-relay.
var redgifsHTTP = &http.Client{Timeout: 30 * time.Second}

func (s *Server) proxyRedgifs(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if !strings.HasSuffix(strings.ToLower(u.Host), ".redgifs.com") {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, raw, nil)
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	// Pretend to be a redgifs.com page loading its own asset.
	req.Header.Set("Referer", "https://www.redgifs.com/")
	req.Header.Set("User-Agent", "binge-server/0.1 (+redgifs-proxy)")
	// Forward Range so the <video> element can seek.
	if rh := r.Header.Get("Range"); rh != "" {
		req.Header.Set("Range", rh)
	}

	resp, err := redgifsHTTP.Do(req)
	if err != nil {
		s.log.Warn("redgifs proxy upstream failed", "url", raw, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{
		"Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "Cache-Control", "Last-Modified", "ETag",
	} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// ── /reddit/proxy ─────────────────────────────────────────────────────
//
// Streams Reddit-hosted image/video assets (i.redd.it, preview.redd.it,
// external-preview.redd.it, v.redd.it) through this daemon. Reasons we
// proxy:
//   - Reddit's CDN sometimes rejects hot-linked images whose Referer
//     isn't reddit.com (especially gallery preview URLs).
//   - UK uni / corporate networks may block reddit subdomains at the
//     firewall; binge-server can egress via Mullvad NL.
//   - Forwarding Range headers lets <video poster> + seeking work.
//
// Allowlist: any host ending in `.redd.it` or `.redditmedia.com`. Same
// "no open-relay" guarantee as the redgifs proxy.
var redditProxyHTTP = &http.Client{Timeout: 30 * time.Second}

func (s *Server) proxyReddit(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	host := strings.ToLower(u.Host)
	if !(strings.HasSuffix(host, ".redd.it") || strings.HasSuffix(host, ".redditmedia.com")) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, raw, nil)
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Referer", "https://www.reddit.com/")
	req.Header.Set("User-Agent", "binge-server/0.2 (+reddit-proxy)")
	if rh := r.Header.Get("Range"); rh != "" {
		req.Header.Set("Range", rh)
	}

	resp, err := redditProxyHTTP.Do(req)
	if err != nil {
		s.log.Warn("reddit proxy upstream failed", "url", raw, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{
		"Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "Cache-Control", "Last-Modified", "ETag",
	} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// ── /healthz ──────────────────────────────────────────────────────────

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	state := map[string]string{}
	rows, err := s.db.QueryContext(r.Context(), `SELECT key, value FROM sync_state`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err == nil {
				state[k] = v
			}
		}
	}
	var performerCount, postCount int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM performers`).Scan(&performerCount)
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM posts`).Scan(&postCount)
	configured := s.store.Get(configstore.KeyStashURL) != "" &&
		s.store.Get(configstore.KeyStashAPIKey) != "" &&
		s.store.Get(configstore.KeyRedditCookie) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"configured":        configured,
		"lastPerformerSync": state["last_performer_sync"],
		"lastPoll":          state["last_poll"],
		"performerCount":    performerCount,
		"postCount":         postCount,
	})
}

// ── /config ──────────────────────────────────────────────────────────
//
// GET returns the public shape of the stored config — never the
// secrets themselves, just whether each is set + the (non-secret)
// Stash URL so the UI can render placeholders accurately.
//
// POST accepts any subset of {stashUrl, stashApiKey, redditSessionCookie}
// — empty/missing fields are ignored. Each non-empty field is
// validated against the live service before being persisted; on
// failure the entire request is rejected.

type configGetResponse struct {
	StashURL        string `json:"stashUrl"`
	StashAPIKeySet  bool   `json:"stashApiKeySet"`
	RedditCookieSet bool   `json:"redditCookieSet"`
}

type configPostRequest struct {
	StashURL            *string `json:"stashUrl,omitempty"`
	StashAPIKey         *string `json:"stashApiKey,omitempty"`
	RedditSessionCookie *string `json:"redditSessionCookie,omitempty"`
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, configGetResponse{
		StashURL:        s.store.Get(configstore.KeyStashURL),
		StashAPIKeySet:  s.store.Get(configstore.KeyStashAPIKey) != "",
		RedditCookieSet: s.store.Get(configstore.KeyRedditCookie) != "",
	})
}

func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	// Block cross-origin browser writes (CSRF) — a malicious page must
	// not be able to POST credentials here. Non-browser callers (the iOS
	// app, curl) send no Origin and are allowed; the web plugin must have
	// its Stash origin in BINGE_ALLOWED_ORIGIN.
	if !originAllowed(r.Header.Get("Origin"), s.allowedOrigins) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked; set BINGE_ALLOWED_ORIGIN to your Stash origin"})
		return
	}
	var req configPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}

	// Restrict the Stash destination to loopback/private/tailnet so a
	// config write can't repoint the stored API key at a public host
	// (credential exfiltration). Public IPs / FQDNs are rejected.
	if req.StashURL != nil && *req.StashURL != "" && !stashURLAllowed(*req.StashURL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stashUrl must be a loopback, private, or tailnet address"})
		return
	}

	// Validate first, persist after. We need the destination URL to be
	// in scope for the Stash API-key probe, so resolve the candidate
	// URL up front.
	stashURL := s.store.Get(configstore.KeyStashURL)
	if req.StashURL != nil && *req.StashURL != "" {
		stashURL = strings.TrimRight(*req.StashURL, "/")
	}

	if req.StashAPIKey != nil && *req.StashAPIKey != "" {
		if err := probeStashAPIKey(r.Context(), stashURL, *req.StashAPIKey); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stash api key rejected: " + err.Error()})
			return
		}
	}

	if req.RedditSessionCookie != nil && *req.RedditSessionCookie != "" {
		if err := probeRedditCookie(r.Context(), *req.RedditSessionCookie); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reddit cookie rejected: " + err.Error()})
			return
		}
	}

	// All present fields validated — persist.
	if req.StashURL != nil {
		if err := s.store.Set(configstore.KeyStashURL, strings.TrimRight(*req.StashURL, "/")); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}
	if req.StashAPIKey != nil {
		if err := s.store.Set(configstore.KeyStashAPIKey, *req.StashAPIKey); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}
	if req.RedditSessionCookie != nil {
		if err := s.store.Set(configstore.KeyRedditCookie, *req.RedditSessionCookie); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// probeStashAPIKey runs a trivial GraphQL query against the given
// Stash URL with the candidate API key. A 200 OK confirms the key is
// valid; anything else (auth error, network failure) is reported back.
func probeStashAPIKey(ctx context.Context, baseURL, apiKey string) error {
	if baseURL == "" {
		return errors.New("stash url not set")
	}
	body, _ := json.Marshal(map[string]any{
		"query": `{ version { version } }`,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ApiKey", apiKey)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("stash returned %d", resp.StatusCode)
	}
	var wrap struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MB cap
	_ = json.Unmarshal(raw, &wrap)
	if len(wrap.Errors) > 0 {
		return errors.New(wrap.Errors[0].Message)
	}
	return nil
}

// probeRedditCookie does a tiny request to oauth.reddit.com's /api/v1/me
// — fast, low-cost, and returns 200 only for a valid session.
func probeRedditCookie(ctx context.Context, cookie string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.reddit.com/api/me.json", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "binge-server/0.2 (config validator)")
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("reddit returned %d", resp.StatusCode)
	}
	// The /api/me.json endpoint returns "{}" for anonymous sessions
	// (no cookie / expired cookie) and the user's account JSON when
	// authenticated. We treat empty `{}` as "not authenticated."
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MB cap
	if bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
		return errors.New("session returned anonymous response")
	}
	return nil
}

// ── /reddit/stories ───────────────────────────────────────────────────

type storyDigest struct {
	PerformerStashID   int       `json:"performerStashId"`
	PerformerName      string    `json:"performerName"`
	PerformerImagePath string    `json:"performerImagePath"`
	PerformerFavorite  bool      `json:"performerFavorite"`
	LatestCreatedUtc   int64     `json:"latestCreatedUtc"`
	PostCount          int       `json:"postCount"`
	Posts              []postRow `json:"posts"`
}

// Max posts attached per performer in the /reddit/stories response.
// Mirrors Reddit's default listing page size.
const storyPostsPerPerformer = 25

func (s *Server) redditStories(w http.ResponseWriter, r *http.Request) {
	sinceUtc, _ := strconv.ParseInt(r.URL.Query().Get("sinceUtc"), 10, 64)

	// 1) Identify performers with content since sinceUtc.
	digestRows, err := s.db.QueryContext(r.Context(), `
		SELECT p.stash_id, p.name, p.image_path, p.favorite,
		       MAX(po.created_utc) AS latest, COUNT(po.reddit_id) AS cnt
		FROM performers p
		JOIN posts po ON po.performer_stash_id = p.stash_id
		WHERE po.created_utc >= ?
		GROUP BY p.stash_id, p.name, p.image_path, p.favorite
		ORDER BY latest DESC`, sinceUtc)
	if err != nil {
		s.log.Error("redditStories digest query", "err", err)
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}

	digests := []storyDigest{}
	for digestRows.Next() {
		var d storyDigest
		var fav int
		if err := digestRows.Scan(&d.PerformerStashID, &d.PerformerName, &d.PerformerImagePath, &fav, &d.LatestCreatedUtc, &d.PostCount); err != nil {
			digestRows.Close()
			s.log.Error("redditStories scan", "err", err)
			http.Error(w, "db", http.StatusInternalServerError)
			return
		}
		d.PerformerFavorite = fav != 0
		digests = append(digests, d)
	}
	digestRows.Close()
	if err := digestRows.Err(); err != nil {
		s.log.Error("redditStories digest iter", "err", err)
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}

	// 2) For each performer, attach their N most-recent posts.
	for i := range digests {
		posts, err := s.queryPostsForPerformer(r.Context(), digests[i].PerformerStashID, sinceUtc, storyPostsPerPerformer)
		if err != nil {
			s.log.Error("redditStories posts query", "stashId", digests[i].PerformerStashID, "err", err)
			http.Error(w, "db", http.StatusInternalServerError)
			return
		}
		digests[i].Posts = posts
	}

	writeJSON(w, http.StatusOK, digests)
}

func (s *Server) queryPostsForPerformer(ctx context.Context, stashID int, sinceUtc int64, limit int) ([]postRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT reddit_id, kind, title, body, media_url, link_url, thumb_url, permalink, domain, is_nsfw, created_utc
		FROM posts
		WHERE performer_stash_id = ? AND created_utc >= ?
		ORDER BY created_utc DESC
		LIMIT ?`, stashID, sinceUtc, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []postRow{}
	for rows.Next() {
		var p postRow
		var nsfw int
		var title, body, media, link, thumb, domain sql.NullString
		if err := rows.Scan(&p.ID, &p.Kind, &title, &body, &media, &link, &thumb, &p.Permalink, &domain, &nsfw, &p.CreatedUtc); err != nil {
			return nil, err
		}
		p.Title = nullStr(title)
		p.Body = nullStr(body)
		p.MediaURL = nullStr(media)
		p.LinkURL = nullStr(link)
		p.ThumbURL = nullStr(thumb)
		p.Domain = nullStr(domain)
		p.IsNSFW = nsfw != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── /reddit/feed/:stashId ─────────────────────────────────────────────

type postRow struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Title      *string `json:"title"`
	Body       *string `json:"body"`
	MediaURL   *string `json:"mediaUrl"`
	LinkURL    *string `json:"linkUrl"`
	ThumbURL   *string `json:"thumbUrl"`
	Permalink  string  `json:"permalink"`
	Domain     *string `json:"domain"`
	IsNSFW     bool    `json:"isNsfw"`
	CreatedUtc int64   `json:"createdUtc"`
}

func (s *Server) redditFeed(w http.ResponseWriter, r *http.Request) {
	stashID, err := strconv.Atoi(chi.URLParam(r, "stashId"))
	if err != nil {
		http.Error(w, "bad stashId", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT reddit_id, kind, title, body, media_url, link_url, thumb_url, permalink, domain, is_nsfw, created_utc
		FROM posts
		WHERE performer_stash_id = ?
		ORDER BY created_utc DESC
		LIMIT ?`, stashID, limit)
	if err != nil {
		s.log.Error("redditFeed query", "err", err)
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []postRow{}
	for rows.Next() {
		var p postRow
		var nsfw int
		var title, body, media, link, thumb, domain sql.NullString
		if err := rows.Scan(&p.ID, &p.Kind, &title, &body, &media, &link, &thumb, &p.Permalink, &domain, &nsfw, &p.CreatedUtc); err != nil {
			s.log.Error("redditFeed scan", "err", err)
			http.Error(w, "db", http.StatusInternalServerError)
			return
		}
		p.Title = nullStr(title)
		p.Body = nullStr(body)
		p.MediaURL = nullStr(media)
		p.LinkURL = nullStr(link)
		p.ThumbURL = nullStr(thumb)
		p.Domain = nullStr(domain)
		p.IsNSFW = nsfw != 0
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// ── /reddit/refresh ───────────────────────────────────────────────────

func (s *Server) redditRefresh(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r.Header.Get("Origin"), s.allowedOrigins) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
		return
	}
	s.refreshMu.Lock()
	if time.Since(s.lastRefreshAt) < refreshCooldown {
		s.refreshMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"queued":            false,
			"cooldownRemaining": (refreshCooldown - time.Since(s.lastRefreshAt)).Seconds(),
		})
		return
	}
	s.lastRefreshAt = time.Now()
	s.refreshMu.Unlock()

	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.poller.PollAll(bg); err != nil {
			s.log.Error("manual refresh failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

// ── helpers ───────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func nullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// corsMiddleware echoes back the request's Origin when it matches the
// allowlist, or "*" when the daemon was started with BINGE_ALLOWED_ORIGIN=*
// (current default for back-compat with the existing Docker deploy).
// A typical user runs binge-server on the same machine as Stash and
// sets BINGE_ALLOWED_ORIGIN=http://localhost:9999 — that locks /config
// down to same-origin POSTs from Stash.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Never echo "*": this API writes credentials, so wildcard CORS
		// is unsafe. Echo the Origin only when it's explicitly allowed
		// (configured allowlist) or loopback (same-host Stash).
		if origin != "" && originAllowed(origin, s.allowedOrigins) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
