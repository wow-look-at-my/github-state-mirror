package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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

	if cfg.WebhookSecret == "" {
		slog.Warn("WEBHOOK_SECRET not set; the /webhook endpoint will reject all deliveries")
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
	gh := ghclient.New()

	// Register all fetchers.
	syncpkg.RegisterAll(mgr, gh, store)

	// Webhook dispatcher.
	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)

	// Background refreshes sign in as a GitHub App (the service holds no static
	// token of its own). Without an app configured, periodic refreshes are
	// disabled; per-request data still works via each caller's Authorization
	// header.
	sessions := buildAppSessions(cfg, gh)

	// Periodic refresher.
	refresher := syncpkg.NewPeriodicRefresher(mgr, cfg.RefreshInterval, sessions)

	// Build router.
	router := api.NewRouter(mgr, store, cfg.WebhookSecret, dispatcher, gh, cfg.AllowedOrigins)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start periodic refresher.
	go refresher.Start(ctx)

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

// buildAppSessions constructs the GitHub App-backed SessionFunc for background
// refreshes, or nil when no app is configured. Misconfiguration (app id set but
// the key missing or unparseable) is logged and disables refreshes rather than
// taking down the request-serving path, which needs no service credential.
func buildAppSessions(cfg config.Config, gh *ghclient.Client) syncpkg.SessionFunc {
	if !cfg.GitHubAppConfigured() {
		slog.Warn("no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY[_PATH]); periodic background refreshes are disabled (per-request data still works via the caller's Authorization header)")
		return nil
	}

	keyPEM, err := cfg.AppPrivateKeyPEM()
	if err != nil {
		slog.Error("GitHub App background refresher disabled", "error", err)
		return nil
	}
	if len(keyPEM) == 0 {
		slog.Error("GitHub App background refresher disabled", "error", "GITHUB_APP_ID is set but no private key was provided (set GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH)")
		return nil
	}

	app, err := ghclient.NewAppAuthenticator(cfg.GitHubAppID, keyPEM, gh)
	if err != nil {
		slog.Error("GitHub App background refresher disabled", "error", err)
		return nil
	}

	// Validate credentials up front so misconfiguration surfaces at startup
	// rather than at the first (6h-later) refresh tick.
	if installs, err := app.Installations(context.Background()); err != nil {
		slog.Warn("could not validate GitHub App credentials", "error", err)
	} else {
		slog.Info("GitHub App authenticated", "app_id", cfg.GitHubAppID, "installations", len(installs))
	}

	return syncpkg.AppSessions(app)
}
