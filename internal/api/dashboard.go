package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// Embedded dashboard assets. Only the files the production page references are
// embedded; src/*.ts and the preview-only demo-data.js are deliberately left
// out. assets/*.js is a BUILD OUTPUT — `npm run build` (tsc) emits it from
// src/*.ts and it is gitignored, so run that before building this package or
// the embed below fails to resolve (CI builds it in the same job, ahead of the
// Go build). A new asset must also be added to newDashboard's hashed-name
// rewrite and to the CI preview job's copied-assets list
// (.github/workflows/ci.yml).
//
//go:embed web/index.html web/assets/app.js web/assets/rate-meter.js web/assets/timeline.js web/assets/style.css
var webFS embed.FS

// dashboard serves the login flow, the static page, and the cache-stats API.
// Its authorization model is by GitHub login (via OAuth session), distinct from
// the bearer-token + per-user partition model that guards the data API. It never
// serves one scope's cached rows to another user — it only reports counts and
// freshness metadata, grouped by login for convenience.
// contentAsset is an embedded asset served at a content-addressed URL — the
// filename embeds a hash of the content. A new deploy with changed JS/CSS yields
// a new URL, so browsers and the CDN/proxy fetch the new file immediately
// instead of serving a stale copy until a cache TTL expires. Because the URL is
// unique per content, it is served with a long-lived, immutable cache header.
type contentAsset struct {
	url         string // e.g. "/assets/app.1a2b3c4d5e.js"
	content     []byte
	contentType string
}

type dashboard struct {
	auth    *auth.Service
	store   *ghdata.Store
	baseURL string
	index   []byte
	assets  []contentAsset
	reqlog  *requestLog
	checker *syncpkg.ConsistencyChecker
	// meter is the passively observed rate-limit store (X-RateLimit-* headers
	// recorded off upstream responses) surfaced on the "Rate limit" tab.
	meter *ratemeter.Store
	// notifier backs the admin-only GET /api/notifications JSON view
	// (subscriber-notification activity + all subscriptions). Nil-safe.
	notifier *notify.Notifier
	// dbPath is the SQLite database file path (DB_PATH), statted per request
	// by handleRequests to report the cache's on-disk footprint.
	dbPath string
	// timeline is the in-memory timed-traffic ring (webhook deliveries +
	// proxied requests) behind the admin-only GET /api/timeline. Nil-safe.
	timeline *reqtimeline.Recorder
	// shapes is the captured request/response shape of uncached traffic
	// (shapes.go) behind the admin-only GET /api/brief. Nil-safe.
	shapes *shapeStore
	// appEvents reports the event types the GitHub App is subscribed to,
	// straight from GitHub. It is what the Webhooks tab's missing-subscription
	// check reads; nil (no App configured) means the check reports nothing
	// rather than guessing from traffic.
	appEvents func(context.Context) ([]string, error)
	// ordering reports what the out-of-order delivery gate has seen (counts,
	// lateness bands, recent refusals). Nil-safe: without it the Webhooks tab
	// simply omits the panel.
	ordering func() syncpkg.OrderingSnapshot
}

func newDashboard(authSvc *auth.Service, store *ghdata.Store, baseURL string, reqlog *requestLog, checker *syncpkg.ConsistencyChecker, meter *ratemeter.Store, notifier *notify.Notifier, dbPath string, timeline *reqtimeline.Recorder, shapes *shapeStore, appEvents func(context.Context) ([]string, error), ordering func() syncpkg.OrderingSnapshot) *dashboard {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		// Embedded at compile time; a read failure is a programmer error.
		panic("read embedded index.html: " + err.Error())
	}
	appJS := mustReadAsset("web/assets/app.js")
	rateMeterJS := mustReadAsset("web/assets/rate-meter.js")
	timelineJS := mustReadAsset("web/assets/timeline.js")
	styleCSS := mustReadAsset("web/assets/style.css")
	appName := hashedAssetName("app", "js", appJS)
	rateMeterName := hashedAssetName("rate-meter", "js", rateMeterJS)
	timelineName := hashedAssetName("timeline", "js", timelineJS)
	cssName := hashedAssetName("style", "css", styleCSS)

	// Rewrite the served HTML to point at the content-addressed URLs. The
	// committed index.html keeps the stable names (assets/app.js) so the
	// backend-free CI styling preview still resolves them; only the HTML the Go
	// server hands out references the hashed URLs.
	served := strings.ReplaceAll(string(index), "assets/app.js", "assets/"+appName)
	served = strings.ReplaceAll(served, "assets/rate-meter.js", "assets/"+rateMeterName)
	served = strings.ReplaceAll(served, "assets/timeline.js", "assets/"+timelineName)
	served = strings.ReplaceAll(served, "assets/style.css", "assets/"+cssName)

	return &dashboard{
		auth:    authSvc,
		store:   store,
		baseURL: strings.TrimRight(baseURL, "/"),
		index:   []byte(served),
		assets: []contentAsset{
			{url: "/assets/" + appName, content: appJS, contentType: "text/javascript; charset=utf-8"},
			{url: "/assets/" + rateMeterName, content: rateMeterJS, contentType: "text/javascript; charset=utf-8"},
			{url: "/assets/" + timelineName, content: timelineJS, contentType: "text/javascript; charset=utf-8"},
			{url: "/assets/" + cssName, content: styleCSS, contentType: "text/css; charset=utf-8"},
		},
		reqlog:    reqlog,
		checker:   checker,
		meter:     meter,
		notifier:  notifier,
		dbPath:    dbPath,
		timeline:  timeline,
		shapes:    shapes,
		appEvents: appEvents,
		ordering:  ordering,
	}
}

func mustReadAsset(name string) []byte {
	b, err := webFS.ReadFile(name)
	if err != nil {
		panic("read embedded asset " + name + ": " + err.Error())
	}
	return b
}

// hashedAssetName returns "<stem>.<hash>.<ext>" where hash is the first 10 hex
// chars of the content's SHA-256 — enough to be collision-free for two files.
func hashedAssetName(stem, ext string, content []byte) string {
	sum := sha256.Sum256(content)
	return stem + "." + hex.EncodeToString(sum[:])[:10] + "." + ext
}

// routes registers the dashboard's routes on r. These sit outside requireAuth:
// they authenticate via the session cookie (or nothing, for the public page),
// never a bearer token.
func (d *dashboard) routes(r chi.Router) {
	r.Get("/", d.serveIndex)

	assetsSub, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		panic("sub embedded assets: " + err.Error())
	}
	// Serve the content-addressed asset URLs with an immutable, long-lived cache;
	// fall back to the stable filenames (assets/app.js) via the file server.
	fileServer := http.StripPrefix("/assets/", http.FileServer(http.FS(assetsSub)))
	r.Handle("/assets/*", d.serveAssets(fileServer))

	r.Get("/login", d.handleLogin)
	r.Get("/auth/callback", d.handleCallback)
	r.Post("/logout", d.handleLogout)

	r.Get("/api/me", d.handleMe)
	r.Get("/api/cache", d.handleCacheStats)
	r.Get("/api/webhooks", d.handleWebhooks)
	r.Get("/api/jobs", d.handleJobs)
	r.Get("/api/requests", d.handleRequests)
	r.Get("/api/timeline", d.handleTimeline)

	// Admin-only: browse the actual cached rows for one scope, run a consistency
	// check that re-fetches the source of truth from GitHub (GET = read-only;
	// POST ?apply=true additionally reconciles the drift), and read the GitHub
	// App's rate-limit status.
	r.Get("/api/cache/data", d.handleCacheData)
	r.Get("/api/cache/check", d.handleCacheCheck)
	r.Post("/api/cache/check", d.handleCacheCheck)
	r.Get("/api/ratelimit", d.handleRateLimit)
	r.Get("/api/notifications", d.handleNotifications)
	r.Get("/api/brief", d.handleBrief)
}

// serveAssets serves the content-addressed asset URLs (immutable cache) and
// delegates everything else under /assets/ to the embedded file server (the
// stable filenames, e.g. assets/app.js).
func (d *dashboard) serveAssets(fileServer http.Handler) http.HandlerFunc {
	byURL := make(map[string]contentAsset, len(d.assets))
	for _, a := range d.assets {
		byURL[a.url] = a
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if a, ok := byURL[r.URL.Path]; ok {
			w.Header().Set("Content-Type", a.contentType)
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			_, _ = w.Write(a.content)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

func (d *dashboard) serveIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(d.index)
}

// ---- request-origin helpers ----

func (d *dashboard) origin(r *http.Request) string {
	if d.baseURL != "" {
		return d.baseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

func (d *dashboard) redirectURI(r *http.Request) string { return d.origin(r) + "/auth/callback" }
func (d *dashboard) secure(r *http.Request) bool        { return strings.HasPrefix(d.origin(r), "https://") }

// ---- OAuth handlers ----

func (d *dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !d.auth.Configured() {
		http.Error(w, "login is not configured on this server", http.StatusServiceUnavailable)
		return
	}
	state, err := auth.RandomState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.StateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   d.secure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.Redirect(w, r, d.auth.AuthCodeURL(d.redirectURI(r), state), http.StatusFound)
}

// signinFailedHTML is the page every callback failure renders. Fixed content
// only — no user-controlled text ever lands in it (details belong in the logs),
// so it is injection-proof by construction. The retry link points at /login,
// which mints a fresh state cookie and restarts the flow cleanly.
const signinFailedHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Sign-in failed</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1rem;">
<h1>Sign-in failed</h1>
<p>Sign-in could not be completed.</p>
<p><a href="/login">Try signing in again</a></p>
</body>
</html>
`

// signinFailed answers a callback failure with the retry page at a 4xx status.
// NEVER a 5xx here: Cloudflare replaces origin 5xx bodies with its own bare
// error page, which stranded the operator on a context-free "502 Bad Gateway"
// dead end when a GitHub exchange failed (incident 2026-07-19T02:15Z). The
// caller logs the actual failure; the browser gets a way to retry.
func signinFailed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(signinFailedHTML))
}

func (d *dashboard) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !d.auth.Configured() {
		http.Error(w, "login is not configured on this server", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		slog.Warn("oauth callback error", "error", e, "description", q.Get("error_description"))
		http.Redirect(w, r, d.origin(r)+"/", http.StatusFound)
		return
	}

	// CSRF: the state in the query must match the state cookie we set at /login.
	state := q.Get("state")
	c, err := r.Cookie(auth.StateCookie)
	if err != nil || c.Value == "" || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		// Name which precondition failed — never the state values themselves.
		switch {
		case state == "":
			slog.Warn("oauth callback rejected: state query param missing")
		case err != nil || c.Value == "":
			slog.Warn("oauth callback rejected: state cookie missing or empty")
		default:
			slog.Warn("oauth callback rejected: state mismatch")
		}
		signinFailed(w)
		return
	}
	// Consume the state cookie.
	http.SetCookie(w, &http.Cookie{Name: auth.StateCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: d.secure(r), SameSite: http.SameSiteLaxMode})

	code := q.Get("code")
	if code == "" {
		slog.Warn("oauth callback rejected: code query param missing")
		signinFailed(w)
		return
	}

	ctx := r.Context()
	token, err := d.auth.Exchange(ctx, code, d.redirectURI(r))
	if err != nil {
		slog.Warn("oauth token exchange failed", "error", err)
		signinFailed(w)
		return
	}
	login, _, err := d.auth.FetchLogin(ctx, token)
	if err != nil {
		slog.Warn("oauth fetch login failed", "error", err)
		signinFailed(w)
		return
	}

	d.auth.SetSession(w, login, d.secure(r))
	http.Redirect(w, r, d.origin(r)+"/", http.StatusFound)
}

func (d *dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	d.auth.ClearSession(w, d.secure(r))
	w.WriteHeader(http.StatusNoContent)
}
