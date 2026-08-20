package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/api"
	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/db"
	"github.com/ordureconnoisseur/binge-server/internal/poller"
	"github.com/ordureconnoisseur/binge-server/internal/pornhub"
	"github.com/ordureconnoisseur/binge-server/internal/reddit"
	"github.com/ordureconnoisseur/binge-server/internal/redgifs"
	"github.com/ordureconnoisseur/binge-server/internal/social"
	"github.com/ordureconnoisseur/binge-server/internal/stash"
	"github.com/ordureconnoisseur/binge-server/internal/twitter"
)

// Version is set at build time via -ldflags "-X main.Version=v0.1.0".
// Defaults to "dev" for ad-hoc `go run .` invocations.
var Version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("binge-server starting", "version", Version)

	cfg := loadConfig(log)

	// Create the DB's parent directory up front. Without this, pointing
	// BINGE_DB_PATH at an unmounted volume (the usual Docker slip: setting
	// /data/binge-server.db but forgetting -v) fails deep inside SQLite as
	// "unable to open database file", which reads like corruption rather
	// than a missing mount.
	if dir := filepath.Dir(cfg.dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("create db directory — is the volume mounted and writable?",
				"dir", dir, "err", err)
			os.Exit(1)
		}
	}

	database, err := db.Open(cfg.dbPath)
	if err != nil {
		log.Error("open db", "path", cfg.dbPath, "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// Live config — populated by the binge UI via POST /config. We
	// optionally seed it from env vars at startup so existing Docker
	// deploys keep working without any UI involvement.
	store, err := configstore.New(database)
	if err != nil {
		log.Error("open config store", "err", err)
		os.Exit(1)
	}
	if v := strings.TrimRight(cfg.stashURL, "/"); v != "" {
		_ = store.SetIfEmpty(configstore.KeyStashURL, v)
	}
	_ = store.SetIfEmpty(configstore.KeyStashAPIKey, cfg.stashAPIKey)
	_ = store.SetIfEmpty(configstore.KeyRedditCookie, cfg.redditCookie)
	_ = store.SetIfEmpty(configstore.KeyXAuthToken, cfg.xAuthToken)
	_ = store.SetIfEmpty(configstore.KeyXCT0, cfg.xCT0)
	_ = store.SetIfEmpty(configstore.KeySocialWriteRoot, cfg.socialWriteRoot)
	_ = store.SetIfEmpty(configstore.KeySocialStashRoot, cfg.socialStashRoot)

	// Clients start with whatever's in the store (possibly empty
	// strings). The poller will read fresh values + push them in via
	// SetCookie / SetCredentials on every tick.
	stashClient := stash.New(store.Get(configstore.KeyStashURL), store.Get(configstore.KeyStashAPIKey))
	redditClient := reddit.New(store.Get(configstore.KeyRedditCookie), cfg.redditUserAgent)
	redgifsClient := redgifs.New(cfg.redditUserAgent)

	// X (Twitter) media client — shells out to gallery-dl, config written
	// next to the SQLite DB (the mounted, writable /data volume).
	twitterClient := twitter.New(filepath.Dir(cfg.dbPath), "gallery-dl")
	if err := twitterClient.SetCookies(store.Get(configstore.KeyXAuthToken), store.Get(configstore.KeyXCT0)); err != nil {
		log.Warn("x cookie setup failed", "err", err)
	}

	// Social "save to Stash" pipeline (downloads + places + tags). Paths
	// come from config; unset = feature off (API 503s, UI hides the button).
	// PornHub pillar — yt-dlp client + its own poller (separate tables, so
	// the reddit pillar is untouched).
	pornhubClient := pornhub.New()

	saverSvc := social.New(stashClient, pornhubClient, log.With("component", "saver"))
	saverSvc.SetPaths(store.Get(configstore.KeySocialWriteRoot), store.Get(configstore.KeySocialStashRoot))

	pornhubPoller := pornhub.NewPoller(
		database, store, stashClient, pornhubClient,
		log.With("component", "pornhub"),
		cfg.performerSyncInterval, cfg.pornhubPollInterval,
	)

	pollerSvc := poller.New(
		database, store, stashClient, redditClient, redgifsClient,
		log.With("component", "poller"),
		cfg.performerSyncInterval, cfg.pollInterval,
	)

	server := api.New(database, store, pollerSvc, stashClient, twitterClient, saverSvc, pornhubClient, log.With("component", "api"), cfg.allowedOrigin, cfg.redditUserAgent)
	server.SetVersion(Version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pollerSvc.Run(ctx)
	go pornhubPoller.Run(ctx)

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           server.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", cfg.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// config is the daemon's startup configuration. With live config in
// SQLite the only required fields are filesystem/network details;
// Stash credentials + Reddit cookie are optional seed values that
// flow into the config store via SetIfEmpty on first boot.
type config struct {
	listenAddr            string
	dbPath                string
	stashURL              string // optional seed
	stashAPIKey           string // optional seed
	redditCookie          string // optional seed
	xAuthToken            string // optional seed (X session cookie)
	xCT0                  string // optional seed (X csrf cookie)
	socialWriteRoot       string // optional seed (where this daemon writes saved media)
	socialStashRoot       string // optional seed (path Stash sees those files at)
	redditUserAgent       string
	pollInterval          time.Duration
	performerSyncInterval time.Duration
	pornhubPollInterval   time.Duration
	allowedOrigin         string
}

func loadConfig(log *slog.Logger) config {
	ua := envOr("REDDIT_USER_AGENT", "binge-server/0.2")
	return config{
		listenAddr:            envOr("BINGE_LISTEN_ADDR", "127.0.0.1:7878"),
		dbPath:                envOr("BINGE_DB_PATH", "binge-server.db"),
		stashURL:              strings.TrimRight(envOr("STASH_URL", "http://localhost:9999"), "/"),
		stashAPIKey:           os.Getenv("STASH_API_KEY"),
		redditCookie:          os.Getenv("REDDIT_SESSION_COOKIE"),
		xAuthToken:            os.Getenv("X_AUTH_TOKEN"),
		xCT0:                  os.Getenv("X_CT0"),
		socialWriteRoot:       os.Getenv("BINGE_SOCIAL_WRITE_ROOT"),
		socialStashRoot:       os.Getenv("BINGE_SOCIAL_STASH_ROOT"),
		redditUserAgent:       ua + " " + Version,
		pollInterval:          envDuration(log, "BINGE_POLL_INTERVAL", 4*time.Hour),
		performerSyncInterval: envDuration(log, "BINGE_PERFORMER_SYNC_INTERVAL", 24*time.Hour),
		// PornHub polling is heavier (yt-dlp per performer), so default to
		// a longer cadence than reddit.
		pornhubPollInterval: envDuration(log, "BINGE_PORNHUB_POLL_INTERVAL", 12*time.Hour),
		// CORS allowlist. Loopback / private / tailnet Stash origins are
		// allowed automatically (zero config for the common self-hosted
		// case). Set this only when Stash is served from a PUBLIC origin
		// (a real domain behind a reverse proxy) — comma-separated, e.g.
		// "https://stash.example.com". A literal "*" is ignored (wildcard
		// CORS on a credential API is unsafe).
		allowedOrigin: os.Getenv("BINGE_ALLOWED_ORIGIN"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(log *slog.Logger, key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Silently falling back hides typos like "4hr" or "240" for hours:
		// the daemon then polls on a cadence the operator didn't choose and
		// has no way to notice. Say so and carry on with the default.
		log.Warn("ignoring unparseable duration, using default",
			"key", key, "value", v, "default", def,
			"hint", "use Go duration syntax, e.g. 30m, 4h, 12h")
		return def
	}
	if d <= 0 {
		log.Warn("ignoring non-positive duration, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return d
}
