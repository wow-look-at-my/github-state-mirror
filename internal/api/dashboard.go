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

// Embeds only the files the production page references.
var webFS embed.FS

// contentAsset is an embedded asset served at a content-addressed URL.
type contentAsset struct {
	url         string // e.g. "/assets/app.1a2b3c4d5e.js"
	content     []byte
	contentType string
}

// dashboard authorizes by GitHub login, never by the bearer-token/reveal model. See docs/dashboard/dashboard.md.
type dashboard struct {
	auth    *auth.Service
	store   *ghdata.Store
	baseURL string
	index   []byte
	assets  []contentAsset
	reqlog  *requestLog
	checker *syncpkg.ConsistencyChecker
	// meter is the passively observed rate-limit store behind the "Rate limit" tab.
	meter *ratemeter.Store
	// notifier backs the admin-only GET /api/notifications JSON view. Nil-safe.
	notifier *notify.Notifier
	// dbPath (DB_PATH) is statted per request to report the cache's on-disk footprint.
	dbPath string
	// timeline is the in-memory timed-traffic ring behind GET /api/timeline. Nil-safe.
	timeline *reqtimeline.Recorder
	// shapes captures uncached traffic's request/response shape for GET /api/brief. Nil-safe.
	shapes *shapeStore
	// appEvents reports the App's subscribed events; nil skips the missing-subscription check.
	appEvents func(context.Context) ([]string, error)
	// ordering reports the out-of-order delivery gate's stats; nil omits the Webhooks tab panel.
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

	// The committed index.html keeps the stable names so the CI preview still resolves them.
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

// routes sit outside requireAuth: they authenticate via the session cookie, never a bearer token.
func (d *dashboard) routes(r chi.Router) {
	r.Get("/", d.serveIndex)

	assetsSub, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		panic("sub embedded assets: " + err.Error())
	}
	// Falls back to the stable filenames (assets/app.js) for anything not content-addressed.
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

	// Admin-only truth browse and consistency check/reconcile; see docs/dashboard/operator-tooling.md.
	r.Get("/api/cache/data", d.handleCacheData)
	r.Get("/api/cache/check", d.handleCacheCheck)
	r.Post("/api/cache/check", d.handleCacheCheck)
	r.Get("/api/ratelimit", d.handleRateLimit)
	r.Get("/api/notifications", d.handleNotifications)
	r.Get("/api/brief", d.handleBrief)
}

// serveAssets serves content-addressed URLs directly, else delegates to fileServer.
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

// signinFailed answers a callback failure with the retry page at 4xx, NEVER 5xx.
// See docs/dashboard/dashboard.md for why.
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
