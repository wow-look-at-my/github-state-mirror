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

	// Resolve the default actor for background tasks.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defaultActor := ""
	if cfg.GitHubToken != "" {
		tokenCtx := ghclient.WithToken(ctx, cfg.GitHubToken)
		login, err := gh.ResolveActor(tokenCtx)
		if err != nil {
			slog.Warn("could not resolve default actor from GITHUB_TOKEN", "error", err)
		} else {
			defaultActor = login
			slog.Info("resolved default actor", "login", defaultActor)
		}
	}

	// Start periodic refresher with actor context.
	bgCtx := actor.WithActor(ctx, defaultActor)
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
