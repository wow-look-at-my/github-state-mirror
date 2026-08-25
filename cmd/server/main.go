package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/api"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/config"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	if cfg.WebhookSecret == "" {
		slog.Warn("WEBHOOK_SECRET not set; the /webhook endpoint will reject all deliveries")
	}

	// Set before any store write.
	ghdata.CacheMaxRows = cfg.CacheMaxRows

	db, err := database.Open(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}

	// Config, not cache: a separate file outside the schema-nuke lifecycle.
	subsPath := cfg.SubscriptionsDBPath
	if subsPath == "" {
		subsPath = notify.DeriveDBPath(cfg.DBPath)
	}
	subsStore, err := notify.Open(subsPath)
	if err != nil {
		slog.Error("open subscriptions database", "error", err, "path", subsPath)
		os.Exit(1)
	}

	// Create components.
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	store := ghdata.NewStore(db)
	gh := ghclient.New()

	// In-memory rate-limit observations for the dashboard's Rate limit tab.
	meter := ratemeter.New()
	gh.SetRateObserver(meter.Observe)

	// In-memory ring of every timed exchange, for the dashboard Timeline chart.
	// see docs/dashboard/timeline-ring.md
	timeline := reqtimeline.New()
	gh.SetExchangeObserver(api.TimelineExchangeObserver(timeline))

	// Register all fetchers.
	syncpkg.RegisterAll(mgr, gh, store)

	// nil when no app is configured; requests always carry their own token.
	app := buildAppAuthenticator(cfg, gh)

	// Applies events in the order they happened, not the order they arrived.
	// see docs/webhooks/ordering.md
	dispatcher := syncpkg.NewWebhookDispatcherWindowed(mgr, store, cfg.WebhookReorderWindow)

	// see docs/notifications.md
	notifier := notify.New(notify.Config{Store: subsStore, Access: store, Timeline: timeline})

	// nil sessions disables periodic refreshes; per-request data is unaffected.
	// Each cycle records an installation's account login as its display name.
	var sessions syncpkg.SessionFunc
	if app != nil {
		recordIdentity := func(ctx context.Context, principal, name string) {
			if err := store.RecordActorIdentity(ctx, principal, name); err != nil {
				slog.Warn("record app-installation identity failed", "principal", principal, "error", err)
			}
		}
		sessions = syncpkg.AppSessions(app, recordIdentity)
	}
	refresher := syncpkg.NewPeriodicRefresher(mgr, cfg.RefreshInterval, sessions)

	// see docs/webhooks/delivery-gaps.md
	replayer := syncpkg.NewDeliveryReplayer(app, store, cfg.ReplayInterval)
	switch {
	case app == nil:
		slog.Warn("GITHUB_APP_ID not set; deliveries GitHub fails to hand over cannot be recovered and will stay missed")
	case cfg.ReplayInterval == 0:
		slog.Warn("WEBHOOK_REPLAY_INTERVAL=0; deliveries GitHub fails to hand over will stay missed")
	}

	// Degrades to "unavailable" when app == nil.
	checker := syncpkg.NewConsistencyChecker(gh, store, fStore, app)

	// Auth service for the web dashboard (GitHub OAuth + signed sessions).
	authSvc := auth.New(auth.Config{
		ClientID:     cfg.OAuthClientID,
		ClientSecret: cfg.OAuthClientSecret,
		SessionKey:   cfg.SessionSecret,
		AdminLogins:  cfg.AdminLogins,
		Observer:     api.TimelineLoginObserver(timeline),
	})
	if !authSvc.Configured() {
		slog.Warn("GITHUB_OAUTH_CLIENT_ID/SECRET not set; the dashboard renders but sign-in is disabled")
	}

	// Built here, not in the router, because shutdown has to Drain it.
	debouncer := api.NewDebouncer(cfg.PassthroughDebounce)
	if w := debouncer.Window(); w > 0 {
		slog.Info("passthrough debouncing enabled", "window", w)
	} else {
		slog.Info("passthrough debouncing disabled; uncacheable reads forward immediately")
	}

	// cfg.DBPath is only statted; data access uses the already-open db handle.
	router := api.NewRouter(mgr, store, cfg.WebhookSecret, dispatcher, gh, cfg.AllowedOrigins, authSvc, cfg.BaseURL, checker, meter, notifier, cfg.DBPath, timeline, debouncer, app)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start periodic refresher.
	go refresher.Start(ctx)

	// A restart is itself a gap window, so the first cycle runs immediately.
	go replayer.Start(ctx)

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
	err = srv.ListenAndServe()

	// Drain detached fetches before closing the DB, or a late write lands on a
	// closed handle. Debounced batches first: draining cuts their pending
	// window short so the answer goes out now instead of after the full hold.
	debouncer.Drain(30 * time.Second)
	if !mgr.Drain(30 * time.Second) {
		slog.Warn("shutdown: in-flight fetches did not drain in time; closing DB anyway")
	}
	// Same rule for detached subscriber-notification deliveries.
	if !notifier.Drain(30 * time.Second) {
		slog.Warn("shutdown: in-flight notifications did not drain in time; closing DBs anyway")
	}
	if cerr := db.Close(); cerr != nil {
		slog.Warn("close database", "error", cerr)
	}
	if cerr := subsStore.Close(); cerr != nil {
		slog.Warn("close subscriptions database", "error", cerr)
	}

	if err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

// buildAppAuthenticator returns nil when unconfigured or misconfigured; the
// request-serving path needs no service credential, so it never fails startup.
func buildAppAuthenticator(cfg config.Config, gh *ghclient.Client) *ghclient.AppAuthenticator {
	if !cfg.GitHubAppConfigured() {
		slog.Warn("no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY[_PATH]); periodic background refreshes, on-demand webhook pulls, and the admin consistency check are disabled (per-request data still works via the caller's Authorization header)")
		return nil
	}

	keyPEM, err := cfg.AppPrivateKeyPEM()
	if err != nil {
		slog.Error("GitHub App disabled", "error", err)
		return nil
	}
	if len(keyPEM) == 0 {
		slog.Error("GitHub App disabled", "error", "GITHUB_APP_ID is set but no private key was provided (set GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH)")
		return nil
	}

	app, err := ghclient.NewAppAuthenticator(cfg.GitHubAppID, keyPEM, gh)
	if err != nil {
		slog.Error("GitHub App disabled", "error", err)
		return nil
	}

	// Validate credentials up front so misconfiguration surfaces at startup
	// rather than at the first refresh tick / webhook delivery.
	if installs, err := app.Installations(context.Background()); err != nil {
		slog.Warn("could not validate GitHub App credentials", "error", err)
	} else {
		slog.Info("GitHub App authenticated", "app_id", cfg.GitHubAppID, "installations", len(installs))
	}

	return app
}
