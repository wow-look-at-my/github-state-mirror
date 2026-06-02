package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/api"
	"github.com/wow-look-at-my/github-state-mirror/internal/config"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

func main() {
	cfg := config.Load()

	if cfg.GitHubToken == "" {
		slog.Warn("GITHUB_TOKEN not set; background refreshes will only work if callers supply Authorization headers")
	}

	db, err := database.Open(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Create components.
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	store := ghdata.NewStore(db)
	gh := ghclient.New(cfg.GitHubToken)

	// Register all fetchers.
	syncpkg.RegisterAll(mgr, gh, store)

	// Webhook dispatcher.
	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)

	// Periodic refresher.
	refresher := syncpkg.NewPeriodicRefresher(mgr, cfg.RefreshInterval)

	// Build router.
	router := api.NewRouter(mgr, store, cfg.WebhookSecret, dispatcher, gh)

	// Background tasks run as the service credential (GITHUB_TOKEN), in its own
	// cache partition keyed by the token's fingerprint — the same scheme used
	// for per-user requests, so background-refreshed data is never served to a
	// different caller.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bgCtx := ctx
	if cfg.GitHubToken != "" {
		bgCtx = ghclient.WithToken(bgCtx, cfg.GitHubToken)
		bgCtx = actor.WithActor(bgCtx, ghclient.Fingerprint(cfg.GitHubToken))
		if login, err := gh.ResolveActor(bgCtx); err != nil {
			slog.Warn("could not validate GITHUB_TOKEN", "error", err)
		} else {
			slog.Info("background refresher authenticated", "login", login)
		}
	}

	// Start periodic refresher with the service credential's context.
	go refresher.Start(bgCtx)

	// Start HTTP server.
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: router,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down")
		cancel()
		srv.Shutdown(context.Background())
	}()

	slog.Info("starting server", "addr", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
