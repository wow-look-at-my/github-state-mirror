package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// Embedded dashboard assets. Only the files the production page references are
// embedded; src/*.ts and the preview-only demo-data.js are deliberately left
// out. assets/app.js is generated from src/app.ts by `npm run build` (tsc).
//
//go:embed web/index.html web/assets/app.js web/assets/style.css
var webFS embed.FS

// dashboard serves the login flow, the static page, and the cache-stats API.
// Its authorization model is by GitHub login (via OAuth session), distinct from
// the bearer-token + fingerprint model that guards the data API. It never serves
// one credential's cached rows to another — it only reports counts and freshness
// metadata, grouped by login for convenience.
type dashboard struct {
	auth    *auth.Service
	store   *ghdata.Store
	baseURL string
	index   []byte
	reqlog  *requestLog
	checker *syncpkg.ConsistencyChecker
}

func newDashboard(authSvc *auth.Service, store *ghdata.Store, baseURL string, reqlog *requestLog, checker *syncpkg.ConsistencyChecker) *dashboard {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		// Embedded at compile time; a read failure is a programmer error.
		panic("read embedded index.html: " + err.Error())
	}
	return &dashboard{auth: authSvc, store: store, baseURL: strings.TrimRight(baseURL, "/"), index: index, reqlog: reqlog, checker: checker}
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
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServer(http.FS(assetsSub))))

	r.Get("/login", d.handleLogin)
	r.Get("/auth/callback", d.handleCallback)
	r.Post("/logout", d.handleLogout)

	r.Get("/api/me", d.handleMe)
	r.Get("/api/cache", d.handleCacheStats)
	r.Get("/api/webhooks", d.handleWebhooks)
	r.Get("/api/requests", d.handleRequests)

	// Admin-only: browse the actual cached rows for one scope, and run a
	// consistency check that re-fetches the source of truth from GitHub.
	r.Get("/api/cache/data", d.handleCacheData)
	r.Get("/api/cache/check", d.handleCacheCheck)
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
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	// Consume the state cookie.
	http.SetCookie(w, &http.Cookie{Name: auth.StateCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: d.secure(r), SameSite: http.SameSiteLaxMode})

	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing oauth code", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	token, err := d.auth.Exchange(ctx, code, d.redirectURI(r))
	if err != nil {
		slog.Warn("oauth token exchange failed", "error", err)
		http.Error(w, "could not complete sign-in", http.StatusBadGateway)
		return
	}
	login, _, err := d.auth.FetchLogin(ctx, token)
	if err != nil {
		slog.Warn("oauth fetch login failed", "error", err)
		http.Error(w, "could not read GitHub identity", http.StatusBadGateway)
		return
	}

	d.auth.SetSession(w, login, d.secure(r))
	http.Redirect(w, r, d.origin(r)+"/", http.StatusFound)
}

func (d *dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	d.auth.ClearSession(w, d.secure(r))
	w.WriteHeader(http.StatusNoContent)
}

// ---- dashboard API ----

type meResponse struct {
	Authenticated   bool   `json:"authenticated"`
	LoginConfigured bool   `json:"login_configured"`
	Login           string `json:"login,omitempty"`
	IsAdmin         bool   `json:"is_admin"`
}

func (d *dashboard) handleMe(w http.ResponseWriter, r *http.Request) {
	resp := meResponse{LoginConfigured: d.auth.Configured()}
	if login, ok := d.auth.Session(r); ok {
		resp.Authenticated = true
		resp.Login = login
		resp.IsAdmin = d.auth.IsAdmin(login)
	}
	writeJSON(w, resp)
}

type webhooksResponse struct {
	Deliveries []ghdata.WebhookDelivery `json:"deliveries"`
}

// handleWebhooks returns the recent webhook deliveries and their dispositions.
// The delivery log is global (it spans every repo/tenant), so — unlike the
// per-scope cache stats — it is restricted to admins, consistent with the
// admin-only "all scopes" view.
func (d *dashboard) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	deliveries, err := d.store.RecentWebhookDeliveries(r.Context(), 100)
	if err != nil {
		slog.Warn("list webhook deliveries failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if deliveries == nil {
		deliveries = []ghdata.WebhookDelivery{}
	}
	writeJSON(w, webhooksResponse{Deliveries: deliveries})
}

// handleRequests returns recent data-API requests and their cache disposition
// (hit / miss / passthrough). Like the webhook log it spans every actor/tenant,
// so — consistent with the admin-only "all scopes" view — it is admin-only.
func (d *dashboard) handleRequests(w http.ResponseWriter, r *http.Request) {
	login, ok := d.auth.Session(r)
	if !ok {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
		return
	}
	if !d.auth.IsAdmin(login) {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}
	writeJSON(w, d.reqlog.snapshot(200))
}

type kindFreshness struct {
	Kind        string           `json:"kind"`
	States      map[string]int64 `json:"states"`
	LastFetched string           `json:"last_fetched,omitempty"`
	// Error is the captured failure reason for a resource of this kind currently
	// in the error state (ErrorKey identifies which one), so the dashboard can
	// show *why* a kind is erroring, not just the count.
	Error    string `json:"error,omitempty"`
	ErrorKey string `json:"error_key,omitempty"`
}

type recentRefresh struct {
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Trigger   string `json:"trigger"`
	StartedAt string `json:"started_at"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

type scopeStats struct {
	Actor    string            `json:"actor"`              // short fingerprint (display)
	ActorID  string            `json:"actor_id,omitempty"` // full partition key (for admin browse/check)
	Login    string            `json:"login"`
	IsSelf   bool              `json:"is_self"`
	LastSeen string            `json:"last_seen,omitempty"`
	Counts   ghdata.DataCounts `json:"counts"`
	Total    int64             `json:"total"`
	Kinds    []kindFreshness   `json:"kinds"`
	Recent   []recentRefresh   `json:"recent,omitempty"`
}

type cacheResponse struct {
	Login      string            `json:"login"`
	IsAdmin    bool              `json:"is_admin"`
	Scope      string            `json:"scope"`
	ScopeCount int               `json:"scope_count"`
	Totals     ghdata.DataCounts `json:"totals"`
	Scopes     []scopeStats      `json:"scopes"`
}

const unknownLogin = "(unknown)"

// scopeInput is one actor to summarize, with its (possibly unknown) identity.
type scopeInput struct {
	actor    string
	login    string
	lastSeen string
}

func (d *dashboard) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	login, ok := d.auth.Session(r)
	if !ok {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
		return
	}
	isAdmin := d.auth.IsAdmin(login)

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "mine"
	}
	if scope == "all" && !isAdmin {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	identities, err := d.store.ListActorIdentities(ctx)
	if err != nil {
		slog.Warn("list actor identities failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	inputs := d.collectInputs(ctx, scope, login, identities)

	resp := cacheResponse{Login: login, IsAdmin: isAdmin, Scope: scope, ScopeCount: len(inputs)}
	detailed := scope != "all" // recent activity only on the focused (mine) view
	for _, in := range inputs {
		s, err := d.buildScope(ctx, in, login, detailed)
		if err != nil {
			slog.Warn("build scope failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp.Totals = addCounts(resp.Totals, s.Counts)
		resp.Scopes = append(resp.Scopes, s)
	}
	if resp.Scopes == nil {
		resp.Scopes = []scopeStats{}
	}
	writeJSON(w, resp)
}

// collectInputs returns the actors to summarize for the requested scope, sorted
// for stable display.
func (d *dashboard) collectInputs(ctx context.Context, scope, login string, identities []dbgen.ActorIdentity) []scopeInput {
	if scope != "all" {
		var inputs []scopeInput
		for _, id := range identities {
			if id.Login == login {
				inputs = append(inputs, scopeInput{actor: id.Actor, login: id.Login, lastSeen: id.LastSeen})
			}
		}
		return inputs
	}

	// Admin "all": every identity, plus any cached actor that lacks an identity
	// row (e.g. the background token before it is recorded).
	seen := make(map[string]bool, len(identities))
	inputs := make([]scopeInput, 0, len(identities))
	for _, id := range identities {
		inputs = append(inputs, scopeInput{actor: id.Actor, login: id.Login, lastSeen: id.LastSeen})
		seen[id.Actor] = true
	}
	if cached, err := d.store.CachedActors(ctx); err != nil {
		slog.Warn("list cached actors failed", "error", err)
	} else {
		for _, a := range cached {
			if !seen[a] {
				inputs = append(inputs, scopeInput{actor: a, login: unknownLogin})
			}
		}
	}
	// Known logins first (case-insensitive), unknowns last, then by actor.
	sort.SliceStable(inputs, func(i, j int) bool {
		ui, uj := inputs[i].login == unknownLogin, inputs[j].login == unknownLogin
		if ui != uj {
			return uj // a known login sorts before an unknown one
		}
		li, lj := strings.ToLower(inputs[i].login), strings.ToLower(inputs[j].login)
		if li != lj {
			return li < lj
		}
		return inputs[i].actor < inputs[j].actor
	})
	return inputs
}

func (d *dashboard) buildScope(ctx context.Context, in scopeInput, selfLogin string, detailed bool) (scopeStats, error) {
	counts, err := d.store.DataCounts(ctx, in.actor)
	if err != nil {
		return scopeStats{}, err
	}
	fresh, err := d.store.FreshnessByKind(ctx, in.actor)
	if err != nil {
		return scopeStats{}, err
	}
	errs, err := d.store.ErrorMessagesByKind(ctx, in.actor)
	if err != nil {
		return scopeStats{}, err
	}

	s := scopeStats{
		Actor:    shortFingerprint(in.actor),
		ActorID:  in.actor,
		Login:    in.login,
		IsSelf:   in.login == selfLogin && in.login != unknownLogin,
		LastSeen: in.lastSeen,
		Counts:   counts,
		Total:    sumCounts(counts),
		Kinds:    groupKinds(fresh, errs),
	}
	if detailed {
		logs, err := d.store.RecentRefreshes(ctx, in.actor, 12)
		if err != nil {
			return scopeStats{}, err
		}
		s.Recent = toRecent(logs)
	}
	return s, nil
}

// groupKinds folds per-(kind,state) rows into one entry per resource kind, with
// a map of state -> count and the most recent fetch time across that kind. When
// a kind has an errored resource, the first captured error message (and its key)
// is attached so the dashboard can show why it failed.
func groupKinds(rows []dbgen.ActorFreshnessByKindRow, errRows []dbgen.ActorErrorMessagesByKindRow) []kindFreshness {
	order := make([]string, 0)
	byKind := make(map[string]*kindFreshness)
	for _, row := range rows {
		kf, ok := byKind[row.ResourceKind]
		if !ok {
			kf = &kindFreshness{Kind: row.ResourceKind, States: map[string]int64{}}
			byKind[row.ResourceKind] = kf
			order = append(order, row.ResourceKind)
		}
		kf.States[row.FetchState] += row.Count
		// last_fetched_at is stored as RFC3339 UTC, so lexical max == latest.
		if lf := asTimeString(row.LastFetched); lf > kf.LastFetched {
			kf.LastFetched = lf
		}
	}
	// Attach the first error message per kind (rows are ordered by kind, key).
	for _, e := range errRows {
		kf, ok := byKind[e.ResourceKind]
		if !ok || kf.Error != "" {
			continue
		}
		kf.Error = e.ErrorMessage.String
		kf.ErrorKey = e.ResourceKey
	}
	out := make([]kindFreshness, 0, len(order))
	for _, k := range order {
		out = append(out, *byKind[k])
	}
	return out
}

func toRecent(logs []dbgen.CacheRefreshLog) []recentRefresh {
	out := make([]recentRefresh, 0, len(logs))
	for _, l := range logs {
		status := "running"
		if l.CompletedAt.Valid {
			if l.Success.Valid && l.Success.Int64 == 1 {
				status = "success"
			} else {
				status = "error"
			}
		}
		out = append(out, recentRefresh{
			Kind:      l.ResourceKind,
			Key:       l.ResourceKey,
			Trigger:   l.TriggeredBy,
			StartedAt: l.StartedAt,
			Status:    status,
			// Surface the captured failure reason (cache_refresh_log.error_message)
			// so the dashboard can show *why* a refresh errored, not just that it did.
			Error: l.ErrorMessage.String,
		})
	}
	return out
}

// asTimeString coerces sqlc's interface{} MAX() result (string, []byte, or nil)
// into a string.
func asTimeString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func shortFingerprint(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

func sumCounts(c ghdata.DataCounts) int64 {
	return c.Repos + c.PullRequests + c.Orgs + c.Users + c.CommitChecks + c.PRFiles + c.BranchComparisons
}

func addCounts(a, b ghdata.DataCounts) ghdata.DataCounts {
	return ghdata.DataCounts{
		Repos:             a.Repos + b.Repos,
		PullRequests:      a.PullRequests + b.PullRequests,
		Orgs:              a.Orgs + b.Orgs,
		Users:             a.Users + b.Users,
		CommitChecks:      a.CommitChecks + b.CommitChecks,
		PRFiles:           a.PRFiles + b.PRFiles,
		BranchComparisons: a.BranchComparisons + b.BranchComparisons,
	}
}
