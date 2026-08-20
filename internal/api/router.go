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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/pornhub"
	"github.com/ordureconnoisseur/binge-server/internal/reddit"
	"github.com/ordureconnoisseur/binge-server/internal/social"
	"github.com/ordureconnoisseur/binge-server/internal/stash"
	"github.com/ordureconnoisseur/binge-server/internal/twitter"
)

// Poller is the slim interface the API needs — a synchronous PollAll.
// The router debounces and dispatches it to a goroutine on /refresh.
type Poller interface {
	PollAll(ctx context.Context) error
	// Re-read credentials into the clients without waiting for a tick.
	ApplyConfig() bool
	// Scan Stash for performers and their Reddit URLs. Polling reads
	// the table this fills, so on a fresh daemon nothing can happen
	// until it has run once.
	SyncPerformers(ctx context.Context) error
}

// PillarPoller is a feed with its own performer scan that should be
// woken when credentials change. PornHub has one; more may follow.
type PillarPoller interface {
	SyncPerformers(ctx context.Context) error
}

type Server struct {
	// Build version, surfaced by /healthz so you can tell what's running
	// without shell access to the box — the first question on any bug
	// report, and the way to spot a deploy that didn't actually restart.
	version string

	db      *sql.DB
	store   *configstore.Store
	poller  Poller
	stash   *stash.Client
	twitter *twitter.Client
	saver   *social.Saver
	pornhub *pornhub.Client
	log     *slog.Logger

	refreshMu     sync.Mutex
	lastRefreshAt time.Time

	xCache     *xFeedCache
	phStreams  *phStreamCache
	phPreviews *phPreviewCache
	phOwned    *phOwnedCache

	allowedOrigins []string // CORS allowlist (parsed); loopback always OK

	// Woken alongside the Reddit poller when credentials change.
	pillars []PillarPoller

	// One wake at a time. A full scan takes minutes and the plugin
	// pushes the Stash key whenever it finds the daemon unconfigured,
	// so without this a couple of page loads could set several
	// concurrent scans of the whole library running against Stash.
	wakeMu      sync.Mutex
	wakeRunning bool
	wakeAgain   bool

	// Rate limit on the key-rotation escape, which costs two requests
	// to Stash and is reachable by anyone on the local network who can
	// present a wrong key.
	rotateMu      sync.Mutex
	lastRotateTry time.Time

	// The User-Agent the poller sends. The cookie probe has to send the
	// same one: Reddit's answer depends on who is asking, so validating
	// with a different identity can accept a cookie that then fails on
	// every poll.
	redditUserAgent string
}

// refreshCooldown — minimum time between /reddit/refresh-triggered
// polls. Manual hammering won't translate to bursts on Reddit.
const refreshCooldown = 30 * time.Second

func New(db *sql.DB, store *configstore.Store, poller Poller, stashClient *stash.Client, twitterClient *twitter.Client, saver *social.Saver, pornhubClient *pornhub.Client, log *slog.Logger, allowedOrigin, redditUserAgent string) *Server {
	return &Server{
		db:             db,
		store:          store,
		poller:         poller,
		stash:          stashClient,
		twitter:        twitterClient,
		saver:          saver,
		pornhub:        pornhubClient,
		log:            log,
		xCache:         newXFeedCache(),
		phStreams:      newPHStreamCache(),
		phPreviews:     newPHPreviewCache(),
		phOwned:        newPHOwnedCache(),
		allowedOrigins: parseOrigins(allowedOrigin),
		// Empty in tests, where the probe is never expected to agree
		// with a poller that does not exist.
		redditUserAgent: redditUserAgent,
	}
}

// reviveHandles un-retires every handle and stamps a new config
// generation, atomically. See the call site for why the pair has to
// succeed or fail together.
func (s *Server) reviveHandles(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE performers SET handle_status='ok' WHERE handle_status != 'ok'`,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_state(key,value) VALUES('config_generation', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// clearPollError forgets the last poll failure once something has
// worked. Kept here rather than in the poller because the wake path is
// the one place a failure can be resolved without a poll cycle running.
func (s *Server) clearPollError() {
	_, _ = s.db.Exec(`DELETE FROM sync_state WHERE key='last_poll_error'`)
}

// claimed reports whether a Stash API key has been stored, which is
// what turns on authentication for every route but /healthz.
func (s *Server) claimed() bool {
	return s.store.Get(configstore.KeyStashAPIKey) != ""
}

// logWarn is log.Warn that tolerates a Server built without a logger.
func (s *Server) logWarn(msg string, args ...any) {
	if s.log == nil {
		return
	}
	s.log.Warn(msg, args...)
}

// AddPillarPoller registers a feed to be woken when config changes.
func (s *Server) AddPillarPoller(p PillarPoller) {
	s.pillars = append(s.pillars, p)
}

// wakePillars makes new credentials count immediately.
//
// Configuring a fresh daemon used to leave it completely idle: the
// performer table was empty, so polling had nobody to poll, and the
// scan that fills it was up to 24 hours away. The UI said Saved, the
// daemon logged nothing, and the only cure was a restart. Nobody would
// guess that, so the daemon does it instead.
//
// Runs detached, on its own context: the request's context is cancelled
// as soon as the response is written, and a full scan outlives that by
// minutes.
func (s *Server) wakePillars() {
	// Runs in a goroutine, so a nil dependency here takes the whole
	// daemon down rather than failing one request. Tests build a Server
	// without a poller, and so could a future caller.
	if s.poller == nil {
		return
	}

	// Collapse concurrent calls into one run, and remember that another
	// arrived so the run repeats rather than being dropped: a wake that
	// began before the newest credentials were stored may already be
	// past the point where it reads them.
	s.wakeMu.Lock()
	if s.wakeRunning {
		s.wakeAgain = true
		s.wakeMu.Unlock()
		return
	}
	s.wakeRunning = true
	s.wakeMu.Unlock()
	defer func() {
		s.wakeMu.Lock()
		s.wakeRunning = false
		s.wakeMu.Unlock()
	}()

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		// ApplyConfig reports whether Reddit is set up, and that answer
		// was being discarded. Polling anyway meant the first config
		// POST, which is Stash only in the normal setup order, fired
		// the whole performer list at Reddit with no cookie: sixty
		// anonymous requests, a 429, and the daemon's own address
		// nearer to being refused. Exactly what the poller's comment
		// warns against.
		redditReady := s.poller.ApplyConfig()
		wakeWorked := true
		if err := s.poller.SyncPerformers(ctx); err != nil {
			s.logWarn("performer sync after config change failed", "err", err)
			wakeWorked = false
		} else if redditReady {
			if err := s.poller.PollAll(ctx); err != nil {
				s.logWarn("poll after config change failed", "err", err)
				wakeWorked = false
			}
		}
		for _, p := range s.pillars {
			if err := p.SyncPerformers(ctx); err != nil {
				s.logWarn("pillar sync after config change failed", "err", err)
			}
		}
		// Only a wake that actually worked says anything. Clearing
		// unconditionally erased the reason for the failure that had
		// just happened, which made a daemon whose performer sync keeps
		// failing look healthy again on /healthz: precisely the thing
		// recording it was for.
		if wakeWorked {
			s.clearPollError()
		}
		cancel()

		s.wakeMu.Lock()
		again := s.wakeAgain
		s.wakeAgain = false
		s.wakeMu.Unlock()
		if !again {
			return
		}
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.corsMiddleware)
	// After CORS so preflights still get their headers, before every
	// route so a new endpoint is guarded by default rather than by
	// remembering to guard it.
	r.Use(s.authMiddleware)
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
	r.Get("/x/feed/{stashId}", s.xFeed)
	r.Get("/x/handle/{handle}", s.xFeedByHandle)
	r.Post("/save", s.saveToStash)
	r.Get("/pornhub/feed/{stashId}", s.pornhubFeed)
	r.Get("/pornhub/stories", s.pornhubStories)
	r.Get("/pornhub/stream/{videoId}", s.pornhubStream)
	r.Head("/pornhub/stream/{videoId}", s.pornhubStream)
	r.Get("/pornhub/thumb", s.pornhubThumb)
	r.Get("/pornhub/preview/{videoId}", s.pornhubPreview)
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
// allowedHostSuffixes builds a CheckRedirect that keeps a proxy client
// on its allowlist across redirects. The initial host is checked in the
// handler; this covers every hop after it, so a 302 from an allowlisted
// CDN cannot walk the daemon to an internal address.
func allowedHostSuffixes(suffixes ...string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		host := strings.ToLower(req.URL.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		for _, suf := range suffixes {
			if host == suf || strings.HasSuffix(host, "."+suf) {
				return nil
			}
		}
		return fmt.Errorf("redirect to disallowed host %q", host)
	}
}

var redgifsHTTP = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: allowedHostSuffixes("redgifs.com"),
}

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
var redditProxyHTTP = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: allowedHostSuffixes("redd.it", "redditmedia.com", "reddit.com"),
}

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

// SetVersion records the build version for /healthz. Called once at
// startup; not safe to call concurrently with serving.
func (s *Server) SetVersion(v string) { s.version = v }

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
	// Per pillar, because one boolean over all three was wrong in both
	// directions. It read true for a daemon holding nothing but bogus
	// seeded values, which is what the container healthcheck polls, so
	// docker ps said healthy while every request to Stash was rejected.
	// And it read false forever for a working PornHub-only setup, since
	// PornHub needs no cookie and the flag demanded one.
	//
	// Stash being reachable is the only thing that can be reported
	// honestly here without making a network call per health check, so
	// the counts and timestamps beside it are what say whether the
	// daemon is doing anything.
	stashSet := s.store.Get(configstore.KeyStashURL) != "" &&
		s.store.Get(configstore.KeyStashAPIKey) != ""
	pillars := map[string]bool{
		"stash":   stashSet,
		"reddit":  stashSet && s.store.Get(configstore.KeyRedditCookie) != "",
		"x":       stashSet && s.store.Get(configstore.KeyXAuthToken) != "" && s.store.Get(configstore.KeyXCT0) != "",
		"pornhub": stashSet,
		"save":    s.saver != nil && s.saver.Configured(),
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": s.version,
		// Kept for older plugin builds that read it. Now means "Stash
		// credentials are present", which is the thing every pillar
		// needs and the only one worth gating on.
		"configured": stashSet,
		"pillars":    pillars,
		// Flattened. /healthz is exempt from authentication entirely,
		// including from a public address, and the stored reason is a
		// raw transport error: it carried the Stash host and port, and
		// in one observed case the remote service's banner. That is the
		// same leak that was closed on /config, sitting on the one
		// route with strictly less protection. Whether polling is
		// failing is the useful part; which host and why is for the
		// logs and for an authenticated caller.
		"lastPollError": func() string {
			if state["last_poll_error"] == "" {
				return ""
			}
			if s.claimed() && credentialMatches(r, s.store.Get(configstore.KeyStashAPIKey)) {
				return state["last_poll_error"]
			}
			return "the last poll failed; see the daemon log"
		}(),
		"lastPerformerSync": state["last_performer_sync"],
		"lastPoll":          state["last_poll"],
		// Masked when no cookie is stored, the way GET /config does it:
		// a daemon that never had one has not had one expire.
		"redditCookieExpiredAt": func() string {
			if s.store.Get(configstore.KeyRedditCookie) == "" {
				return ""
			}
			return state["reddit_cookie_expired_at"]
		}(),
		"performerCount": performerCount,
		"postCount":      postCount,
	})
}

// Largest acceptable POST /config body. Every field is a URL, a key or
// a cookie.
const maxConfigBody = 256 << 10

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
	XCookiesSet     bool   `json:"xCookiesSet"`
	// RFC3339 time the poller first found the Reddit cookie rejected,
	// empty when it is working. A cookie set months ago goes stale with
	// no visible symptom other than stories quietly stopping, so the UI
	// needs to be told rather than the user having to guess.
	RedditCookieExpiredAt string `json:"redditCookieExpiredAt,omitempty"`
	// Social "save to Stash" library roots (not secret). Configured =
	// both set.
	SocialWriteRoot      string `json:"socialWriteRoot"`
	SocialStashRoot      string `json:"socialStashRoot"`
	SocialSaveConfigured bool   `json:"socialSaveConfigured"`
}

type configPostRequest struct {
	StashURL            *string `json:"stashUrl,omitempty"`
	StashAPIKey         *string `json:"stashApiKey,omitempty"`
	RedditSessionCookie *string `json:"redditSessionCookie,omitempty"`
	// X (Twitter) session cookies — must be set together.
	XAuthToken *string `json:"xAuthToken,omitempty"`
	XCT0       *string `json:"xCt0,omitempty"`
	// Social library roots (where this daemon writes / where Stash sees it).
	SocialWriteRoot *string `json:"socialWriteRoot,omitempty"`
	SocialStashRoot *string `json:"socialStashRoot,omitempty"`
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configGetResponse{
		StashURL:             s.store.Get(configstore.KeyStashURL),
		StashAPIKeySet:       s.store.Get(configstore.KeyStashAPIKey) != "",
		RedditCookieSet:      s.store.Get(configstore.KeyRedditCookie) != "",
		XCookiesSet:          s.store.Get(configstore.KeyXAuthToken) != "" && s.store.Get(configstore.KeyXCT0) != "",
		SocialWriteRoot:      s.store.Get(configstore.KeySocialWriteRoot),
		SocialStashRoot:      s.store.Get(configstore.KeySocialStashRoot),
		SocialSaveConfigured: s.saver != nil && s.saver.Configured(),
		// Only meaningful while a cookie is actually stored: clearing the
		// cookie should read as "not set up", not as "expired".
		RedditCookieExpiredAt: func() string {
			if s.store.Get(configstore.KeyRedditCookie) == "" {
				return ""
			}
			return s.syncState(r.Context(), "reddit_cookie_expired_at")
		}(),
	})
}

// syncState reads one row from the poller's sync_state table. Missing key
// and db error are both "" — callers treat absence as the healthy case.
func (s *Server) syncState(ctx context.Context, key string) string {
	var v string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, key).Scan(&v)
	return v
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
	// Cap the body. On a daemon nobody has claimed yet this handler is
	// reachable without a credential, and an uncapped decode turned a
	// single 300 MB POST into 1.5 GB resident, which in a
	// memory-limited container is an OOM kill from an unauthenticated
	// caller. Config is a handful of short strings; a quarter of a
	// megabyte is already generous.
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBody)
	// Held until the end: a failure here must not skip the fields that
	// arrived in the same request.
	var reviveErr error
	var req configPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}

	// The other half of the first-run exemption in authMiddleware. An
	// unconfigured daemon reached from a public address may be claimed,
	// but only by a request that establishes the Stash API key, which is
	// probed against a private Stash below. Without this a caller could
	// use the exemption to store a Reddit cookie and nothing else, and
	// so occupy a daemon it never proved it owns.
	firstRunPublicClaim := s.store.Get(configstore.KeyStashAPIKey) == "" && !requestFromPrivateIP(r)
	if firstRunPublicClaim && (req.StashAPIKey == nil || *req.StashAPIKey == "") {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the first request to an unconfigured binge-server reached from a public address must set the Stash API key",
		})
		return
	}

	// Restrict the Stash destination to loopback/private/tailnet so a
	// config write can't repoint the stored API key at a public host
	// (credential exfiltration). Public IPs / FQDNs are rejected.
	if req.StashURL != nil && *req.StashURL != "" {
		if problem := stashURLProblem(*req.StashURL); problem != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": problem})
			return
		}
	}

	// Validate first, persist after. We need the destination URL to be
	// in scope for the Stash API-key probe, so resolve the candidate
	// URL up front.
	stashURL := s.store.Get(configstore.KeyStashURL)
	if req.StashURL != nil && *req.StashURL != "" {
		stashURL = strings.TrimRight(*req.StashURL, "/")
	}

	if req.StashAPIKey != nil && *req.StashAPIKey != "" {
		// A public caller claiming an unconfigured daemon is trusted
		// only because the key it presents is proven to work. But a
		// Stash with authentication switched off answers 200 to any
		// key, and so does any other private endpoint that happens to
		// return JSON, so a bare success proves nothing about the
		// caller. Probe again with a key that cannot be valid: if that
		// also passes, the target is not checking, and the claim is
		// refused rather than handing the daemon to whoever asked.
		//
		// Scoped to the public first-run claim on purpose. An owner on
		// their own LAN running an authless Stash is doing something
		// normal and must still be able to configure the daemon.
		if firstRunPublicClaim {
			if err := probeStashAPIKey(r.Context(), stashURL, bogusProbeKey); err == nil {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "that Stash accepts any API key, so it cannot prove who you are. Configure this daemon from its own network, or seed STASH_API_KEY.",
				})
				return
			}
		}
		if err := probeStashAPIKey(r.Context(), stashURL, *req.StashAPIKey); err != nil {
			// The raw error names the dial result, which tells an
			// off-network caller whether a host and port are open. Fine
			// for someone already on the network and holding a key;
			// an unauthenticated port scanner otherwise.
			detail := err.Error()
			// The raw dial result tells a caller whether a host and
			// port are open, which turns this handler into a port
			// scanner for anyone who can reach it. On an unclaimed
			// daemon that is anyone on the network, since no credential
			// is required yet.
			if firstRunPublicClaim || !s.claimed() {
				// Reaching Stash and being refused is not a secret from
				// someone who already reached this daemon, and calling
				// it a network problem sent people with a mistyped key
				// to go and check their firewall. What must not leak is
				// which hosts and ports are open, so only the dial
				// failures are flattened.
				if errors.Is(err, errStashRejectedKey) {
					detail = "that Stash rejected the key"
				} else {
					detail = "could not reach that Stash"
				}
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stash api key rejected: " + detail})
			return
		}
	}

	if req.RedditSessionCookie != nil && *req.RedditSessionCookie != "" {
		// Cheap and specific first. Reddit answers 403 to anything it
		// does not recognise, so without this a typo and a blocked
		// address are the same message.
		if problem := reddit.CookieShapeProblem(*req.RedditSessionCookie); problem != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reddit cookie rejected: " + problem})
			return
		}
		if err := probeRedditCookie(r.Context(), *req.RedditSessionCookie, s.redditUserAgent); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reddit cookie rejected: " + err.Error()})
			return
		}
	}

	// All present fields validated — persist.
	if req.StashURL != nil {
		if *req.StashURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "stashUrl cannot be cleared. Send the address of your Stash instead",
			})
			return
		}
		if err := s.store.Set(configstore.KeyStashURL, strings.TrimRight(*req.StashURL, "/")); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}
	if req.StashAPIKey != nil {
		// Emptying the key is not a setting, it is a downgrade: the
		// daemon drops back to first-run, where anyone on the local
		// network can rewrite its config and claim it. Nothing in the
		// UI does this, and a field posted blank by accident should not
		// be able to either.
		if *req.StashAPIKey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "stashApiKey cannot be cleared, because the daemon would fall back to accepting anyone on your network. Send a working key instead",
			})
			return
		}
		if err := s.store.Set(configstore.KeyStashAPIKey, *req.StashAPIKey); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}
	// An empty string is a request to forget the cookie, not a no-op.
	// The pointer type exists precisely to tell absent from empty, and
	// answering ok while keeping the old value is the worst of both.
	//
	// Down here with the other writes, not up with the checks. Clearing
	// it early meant a request that was then rejected for an unrelated
	// field had still emptied the cookie, so a caller got a 400 and a
	// broken Reddit pillar at the same time. This handler promises that
	// a rejected request changes nothing.
	if req.RedditSessionCookie != nil && *req.RedditSessionCookie == "" {
		if err := s.store.Set(configstore.KeyRedditCookie, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
		_, _ = s.db.ExecContext(r.Context(),
			`DELETE FROM sync_state WHERE key='reddit_cookie_expired_at'`)
	}
	if req.RedditSessionCookie != nil && *req.RedditSessionCookie != "" {
		if err := s.store.Set(configstore.KeyRedditCookie, reddit.NormalizeCookie(*req.RedditSessionCookie)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
		// This cookie was probed against reddit above, so it works now.
		// Clear the expiry marker immediately rather than making the user
		// stare at a stale warning until the next poll tick, up to 4h away.
		_, _ = s.db.ExecContext(r.Context(),
			`DELETE FROM sync_state WHERE key='reddit_cookie_expired_at'`)
		// Give every retired handle another chance, and tell any poll
		// cycle already under way that its verdicts were reached under
		// credentials nobody is using now, so it does not write them
		// back over what was just revived.
		//
		// One transaction, and its failure is reported. These were two
		// statements whose errors were both discarded, and the second
		// is the guard that protects the first: under contention it is
		// exactly the write most likely to find the database busy,
		// since a poll cycle running is the precondition for needing
		// it. Losing it silently meant the user was told their cookie
		// was saved while the in-flight cycle re-retired the whole
		// library, which is the damage this code exists to undo.
		// Recorded, not returned. Returning here skipped the X cookies
		// and the library roots that arrived in the same request, so a
		// settings page that posts every field on Save silently lost
		// them, and the message named only the cookie. The rest of the
		// request is finished and the failure is surfaced at the end.
		if err := s.reviveHandles(r.Context()); err != nil {
			s.logWarn("cookie saved but handles could not be revived", "err", err)
			reviveErr = err
		}
	}
	// X cookies — both must be provided together (auth_token is useless
	// without the matching ct0 csrf token). Persisted then pushed into the
	// twitter client so the change takes effect without a restart.
	if req.XAuthToken != nil || req.XCT0 != nil {
		if req.XAuthToken == nil || req.XCT0 == nil || *req.XAuthToken == "" || *req.XCT0 == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "xAuthToken and xCt0 must be set together"})
			return
		}
		if err := s.store.Set(configstore.KeyXAuthToken, *req.XAuthToken); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
		if err := s.store.Set(configstore.KeyXCT0, *req.XCT0); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
		s.applyXCookies()
	}
	// Social library roots — persisted then pushed into the saver so the
	// change takes effect without a restart.
	if req.SocialWriteRoot != nil {
		if err := s.store.Set(configstore.KeySocialWriteRoot, strings.TrimSpace(*req.SocialWriteRoot)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}
	if req.SocialStashRoot != nil {
		if err := s.store.Set(configstore.KeySocialStashRoot, strings.TrimSpace(*req.SocialStashRoot)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
	}
	if (req.SocialWriteRoot != nil || req.SocialStashRoot != nil) && s.saver != nil {
		s.saver.SetPaths(
			s.store.Get(configstore.KeySocialWriteRoot),
			s.store.Get(configstore.KeySocialStashRoot),
		)
	}

	if reviveErr != nil {
		// Everything else in the request has been applied by now, so
		// this reports the one part that did not.
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "everything was saved, but re-enabling your retired performers failed. Save your cookie again once the daemon is idle",
		})
		return
	}
	// Credentials just changed, so make them count now rather than at
	// some point in the next day.
	if req.StashURL != nil || req.StashAPIKey != nil || req.RedditSessionCookie != nil {
		go s.wakePillars()
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// errStashRejectedKey means Stash answered and refused the key, as
// opposed to never being reached at all. The difference decides what a
// first-run caller is told, so it is worth a sentinel.
var errStashRejectedKey = errors.New("stash rejected the api key")

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
	client := &http.Client{
		Timeout: 8 * time.Second,
		// Do not follow redirects, exactly as probeRedditCookie does.
		// Go drops Authorization and Cookie when a redirect crosses to
		// another host, but ApiKey is a custom header and is copied to
		// whatever the target is, public hosts included. A private host
		// that 302s was therefore enough to walk the real Stash key out
		// to an address of the redirecting host's choosing.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: stash returned %d", errStashRejectedKey, resp.StatusCode)
	}
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
		return fmt.Errorf("%w: %s", errStashRejectedKey, wrap.Errors[0].Message)
	}
	return nil
}

// bogusProbeKey is offered to a Stash to see whether it rejects anything
// at all. It is deliberately not a plausible key, and is never stored.
const bogusProbeKey = "binge-server-negative-control-not-a-real-key"

// probeRedditCookie checks that a cookie will work for the thing the
// daemon actually does with it, which is two separate questions.
//
// The first is whether Reddit knows who we are. /api/v1/me answers "{}"
// to an anonymous caller, which is what a cookie that never made it
// into a valid header looks like.
//
// The second is whether the listing endpoints will answer us at all.
// They are edge-blocked far more aggressively than the identity route,
// so checking only the first accepts a cookie that then fails on every
// poll, and the daemon looks configured while nothing arrives. That is
// the shape of the bug this was changed to catch. Both requests carry
// the poller's User-Agent for the same reason: Reddit's answer depends
// on who is asking.
func probeRedditCookie(ctx context.Context, cookie, userAgent string) error {
	if userAgent == "" {
		userAgent = "binge-server (config validator)"
	}
	// Whatever the settings page was given, send what the poller sends.
	header := reddit.NormalizeCookie(cookie)
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	get := func(url string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Cookie", header)
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MB cap
		return resp.StatusCode, raw, nil
	}

	code, raw, err := get("https://www.reddit.com/api/me.json")
	if err != nil {
		return err
	}
	switch {
	case code == 200 && bytes.Equal(bytes.TrimSpace(raw), []byte("{}")):
		return errors.New("session returned anonymous response")
	case code == 403:
		return errors.New("reddit refused the request (403). If this cookie works in your browser, reddit is refusing the address binge-server connects from, which hosted servers and some VPN exits hit")
	case code != 200:
		return fmt.Errorf("reddit returned %d", code)
	}

	// Same family of URL the poller fetches per performer.
	code, _, err = get("https://www.reddit.com/r/all/new.json?limit=1&raw_json=1")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("reddit accepted the cookie but answered %d to a listing, which is what the poller fetches, so stories would not arrive", code)
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
		// ApiKey must be listed or the browser's preflight rejects every
		// authenticated call before it is sent.
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, ApiKey, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
