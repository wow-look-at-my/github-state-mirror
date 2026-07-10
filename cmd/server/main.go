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

	// Subscriber-notification config DB: a SEPARATE SQLite file, deliberately
	// outside the cache DB's SchemaVersion nuke-and-recreate lifecycle —
	// subscriptions are configuration, not disposable cached state.
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

	// Passive rate-limit observation: every upstream GitHub response's
	// X-RateLimit-* headers land in this in-memory meter (the dashboard's
	// admin "Rate limit" tab). In-memory like the request log — a live view,
	// not an audit log; it resets on restart. The ghclient hook covers the
	// client's own calls; the api layer feeds the proxy/fetch/probe paths.
	meter := ratemeter.New()
	gh.SetRateObserver(meter.Observe)

	// Register all fetchers.
	syncpkg.RegisterAll(mgr, gh, store)

	// The service's only credential: a GitHub App (there is no static service
	// token). nil when no app is configured. It signs in for the periodic
	// background refreshes and is the source-of-truth fetcher for the admin
	// consistency check. Requests never use it — they carry the caller's own
	// token — and the webhook dispatcher never fetches at all (payloads apply
	// straight to global truth).
	app := buildAppAuthenticator(cfg, gh)

	// Webhook dispatcher: applies every stateful event to global truth.
	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)

	// Subscriber notifier: after each dispatched delivery it POSTs signed
	// notifications to matching subscriptions, reveal-gated per principal
	// (public repo or live grant — fail closed). Deliveries run detached and
	// are drained at shutdown before the DBs close.
	notifier := notify.New(notify.Config{Store: subsStore, Access: store})

	// Periodic refresher. Without an app configured, sessions is nil and periodic
	// refreshes are disabled; per-request data still works via each caller's
	// Authorization header.
	var sessions syncpkg.SessionFunc
	if app != nil {
		sessions = syncpkg.AppSessions(app)
	}
	refresher := syncpkg.NewPeriodicRefresher(mgr, cfg.RefreshInterval, sessions)

	// Consistency checker for the admin dashboard (re-fetches from GitHub via the
	// App and diffs against the cache). Degrades to "unavailable" when app == nil.
	checker := syncpkg.NewConsistencyChecker(gh, store, fStore, app)

	// Auth service for the web dashboard (GitHub OAuth + signed sessions).
	authSvc := auth.New(auth.Config{
		ClientID:     cfg.OAuthClientID,
		ClientSecret: cfg.OAuthClientSecret,
		SessionKey:   cfg.SessionSecret,
		AdminLogins:  cfg.AdminLogins,
	})
	if !authSvc.Configured() {
		slog.Warn("GITHUB_OAUTH_CLIENT_ID/SECRET not set; the dashboard renders but sign-in is disabled")
	}

	// Build router.
	router := api.NewRouter(mgr, store, cfg.WebhookSecret, dispatcher, gh, cfg.AllowedOrigins, authSvc, cfg.BaseURL, checker, meter, notifier)

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
	err = srv.ListenAndServe()

	// Shutdown ordering: the listener is closed (no new requests), the
	// periodic refresher's context is canceled, but DETACHED fetches
	// (freshness.Manager runs each fetch on a cancel-severed context so an
	// impatient client can't kill shared work) may still be writing. Drain
	// them BEFORE closing the database, or a late metadata write lands on a
	// closed handle. Bounded so a wedged upstream cannot hold shutdown hostage
	// past the fetch safety timeout.
	if !mgr.Drain(30 * time.Second) {
		slog.Warn("shutdown: in-flight fetches did not drain in time; closing DB anyway")
	}
	// Same rule for detached subscriber-notification deliveries: stop retries,
	// wait out in-flight POSTs and their outcome writes, THEN close the DBs.
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

// buildAppAuthenticator constructs the GitHub App authenticator that signs the
// service's background work (periodic refreshes, the webhook dispatcher's
// on-demand repo pulls, and the admin consistency check), or nil when no app is
// configured. Misconfiguration (app id set but the key missing or unparseable)
// is logged and disables that work rather than taking down the request-serving
// path, which needs no service credential.
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
