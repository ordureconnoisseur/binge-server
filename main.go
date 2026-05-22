package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/api"
	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/db"
	"github.com/ordureconnoisseur/binge-server/internal/poller"
	"github.com/ordureconnoisseur/binge-server/internal/reddit"
	"github.com/ordureconnoisseur/binge-server/internal/redgifs"
	"github.com/ordureconnoisseur/binge-server/internal/stash"
)

// Version is set at build time via -ldflags "-X main.Version=v0.1.0".
// Defaults to "dev" for ad-hoc `go run .` invocations.
var Version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("binge-server starting", "version", Version)

	cfg := loadConfig()

	database, err := db.Open(cfg.dbPath)
	if err != nil {
		log.Error("open db", "err", err)
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

	// Clients start with whatever's in the store (possibly empty
	// strings). The poller will read fresh values + push them in via
	// SetCookie / SetCredentials on every tick.
	stashClient := stash.New(store.Get(configstore.KeyStashURL), store.Get(configstore.KeyStashAPIKey))
	redditClient := reddit.New(store.Get(configstore.KeyRedditCookie), cfg.redditUserAgent)
	redgifsClient := redgifs.New(cfg.redditUserAgent)

	pollerSvc := poller.New(
		database, store, stashClient, redditClient, redgifsClient,
		log.With("component", "poller"),
		cfg.performerSyncInterval, cfg.pollInterval,
	)

	server := api.New(database, store, pollerSvc, log.With("component", "api"), cfg.allowedOrigin)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pollerSvc.Run(ctx)

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
	redditUserAgent       string
	pollInterval          time.Duration
	performerSyncInterval time.Duration
	allowedOrigin         string
}

func loadConfig() config {
	ua := envOr("REDDIT_USER_AGENT", "binge-server/0.2")
	return config{
		listenAddr:            envOr("BINGE_LISTEN_ADDR", "127.0.0.1:7878"),
		dbPath:                envOr("BINGE_DB_PATH", "binge-server.db"),
		stashURL:              strings.TrimRight(envOr("STASH_URL", "http://localhost:9999"), "/"),
		stashAPIKey:           os.Getenv("STASH_API_KEY"),
		redditCookie:          os.Getenv("REDDIT_SESSION_COOKIE"),
		redditUserAgent:       ua + " " + Version,
		pollInterval:          envDuration("BINGE_POLL_INTERVAL", 4*time.Hour),
		performerSyncInterval: envDuration("BINGE_PERFORMER_SYNC_INTERVAL", 24*time.Hour),
		// Default "*" keeps the existing Docker deploy working. The
		// public README sets this to http://localhost:9999 to lock
		// the daemon to same-origin POSTs from Stash.
		allowedOrigin: envOr("BINGE_ALLOWED_ORIGIN", "*"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
