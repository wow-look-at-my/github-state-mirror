package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// configuredAuth returns an auth.Service wired to a stub GitHub OAuth + API so
// the callback flow can run without the real GitHub. The stub /user always
// reports login "PazerOP".
func configuredAuth(t *testing.T) *auth.Service {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_x"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "PazerOP", "avatar_url": "a"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return auth.New(auth.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		SessionKey:   []byte("test-session-key"),
		AdminLogins:  map[string]bool{"pazerop": true},
		TokenURL:     srv.URL + "/token",
		APIBaseURL:   srv.URL,
	})
}

func mintSession(t *testing.T, svc *auth.Service, login string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	svc.SetSession(rec, login, false)
	cookies := rec.Result().Cookies()
	require.NotEmpty(t, cookies)
	return cookies[0]
}

// seedScope populates a cache scope (actor fingerprint) with a little data, a
// freshness row, and an identity mapping to login.
func seedScope(t *testing.T, store *ghdata.Store, db *sql.DB, fp, login string) {
	t.Helper()
	ctx := actor.WithActor(context.Background(), fp)
	require.NoError(t, store.UpsertUser(ctx, dbgen.User{Login: login, AvatarUrl: "a", Url: "u"}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "o", Name: "r", NameWithOwner: "o/r", Url: "u"}))
	require.NoError(t, store.RecordActorIdentity(context.Background(), fp, login))
	seedFreshness(t, db, fp, "org_repos", "o")
}

func seedFreshness(t *testing.T, db *sql.DB, fp, kind, key string) {
	t.Helper()
	now := time.Now().UTC()
	fs := freshness.NewStore(db)
	require.NoError(t, fs.Upsert(context.Background(), &freshness.Metadata{
		ResourceID:    freshness.ResourceID{Actor: fp, Kind: kind, Key: key},
		State:         freshness.StateFresh,
		LastFetchedAt: &now,
		ExpiresAt:     &now,
	}))
}

func TestDashboard_MeUnauthenticated(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/api/me", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body meResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.Authenticated)
	assert.True(t, body.LoginConfigured)
	assert.False(t, body.IsAdmin)
}

func TestDashboard_MeAuthenticated(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body meResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(t, body.Authenticated)
	assert.Equal(t, "PazerOP", body.Login)
	assert.True(t, body.IsAdmin)
}

func TestDashboard_CacheMine(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedScope(t, store, db, "fp-octocat", "octocat")

	req := httptest.NewRequest("GET", "/api/cache?scope=mine", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp cacheResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "octocat", resp.Login)
	assert.Equal(t, "mine", resp.Scope)
	assert.Equal(t, 1, resp.ScopeCount)
	require.Len(t, resp.Scopes, 1)
	assert.Equal(t, "octocat", resp.Scopes[0].Login)
	assert.True(t, resp.Scopes[0].IsSelf)
	assert.Equal(t, int64(1), resp.Scopes[0].Counts.Repos)
	assert.Equal(t, int64(1), resp.Scopes[0].Counts.Users)
	assert.Equal(t, int64(1), resp.Totals.Repos)
	// org_repos freshness row should surface.
	require.NotEmpty(t, resp.Scopes[0].Kinds)
	assert.Equal(t, "org_repos", resp.Scopes[0].Kinds[0].Kind)
	assert.Equal(t, int64(1), resp.Scopes[0].Kinds[0].States["fresh"])
}

func TestDashboard_CacheMine_Empty(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/cache", nil) // default scope=mine
	req.AddCookie(mintSession(t, svc, "nobody"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp cacheResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.ScopeCount)
	assert.Equal(t, []scopeStats{}, resp.Scopes)
}

func TestDashboard_CacheUnauthenticated(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/api/cache?scope=mine", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDashboard_CacheAll_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedScope(t, store, db, "fp-octocat", "octocat")

	req := httptest.NewRequest("GET", "/api/cache?scope=all", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_CacheAll_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedScope(t, store, db, "fp-octocat", "octocat")
	seedScope(t, store, db, "fp-pazer", "PazerOP")
	// An orphan scope: cache metadata but no identity row.
	seedFreshness(t, db, "fp-orphan", "pr_files", "o/r/1")

	req := httptest.NewRequest("GET", "/api/cache?scope=all", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp cacheResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "all", resp.Scope)
	assert.Equal(t, 3, resp.ScopeCount)

	byLogin := map[string]scopeStats{}
	for _, s := range resp.Scopes {
		byLogin[s.Login] = s
	}
	require.Contains(t, byLogin, "octocat")
	require.Contains(t, byLogin, "PazerOP")
	require.Contains(t, byLogin, "(unknown)")
	assert.True(t, byLogin["PazerOP"].IsSelf)
	assert.False(t, byLogin["octocat"].IsSelf)
	assert.False(t, byLogin["(unknown)"].IsSelf)
	// Admin "all" omits the recent-activity detail.
	assert.Empty(t, byLogin["octocat"].Recent)
}

func TestDashboard_Webhooks_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	require.NoError(t, store.RecordWebhookDelivery(context.Background(), ghdata.WebhookDelivery{
		DeliveryID:  "abc123",
		EventType:   "pull_request",
		Action:      "opened",
		Repo:        "o/r",
		Disposition: webhook.DispApplied,
		Detail:      "upserted PR #5",
		Actors:      2,
	}))

	req := httptest.NewRequest("GET", "/api/webhooks", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp webhooksResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Deliveries, 1)
	assert.Equal(t, "abc123", resp.Deliveries[0].DeliveryID)
	assert.Equal(t, webhook.DispApplied, resp.Deliveries[0].Disposition)
	assert.Equal(t, int64(2), resp.Deliveries[0].Actors)
}

func TestDashboard_Webhooks_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedScope(t, store, db, "fp-octocat", "octocat")

	req := httptest.NewRequest("GET", "/api/webhooks", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_Webhooks_Unauthenticated(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/webhooks", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDashboard_Login_Redirects(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusFound, w.Code)
	loc := w.Header().Get("Location")
	u, err := url.Parse(loc)
	require.NoError(t, err)
	assert.Equal(t, "github.com", u.Host)
	assert.NotEmpty(t, u.Query().Get("state"))

	// State cookie must be set.
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.StateCookie {
			found = true
		}
	}
	assert.True(t, found, "login must set the oauth state cookie")
}

func TestDashboard_Login_NotConfigured(t *testing.T) {
	router, _ := setupTestRouter(t) // unconfigured auth
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestDashboard_Logout(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared, "logout must clear the session cookie")
}

func TestDashboard_CallbackFlow(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	// Step 1: /login to obtain a state + its cookie.
	loginReq := httptest.NewRequest("GET", "/login", nil)
	loginW := httptest.NewRecorder()
	router.ServeHTTP(loginW, loginReq)
	require.Equal(t, http.StatusFound, loginW.Code)
	u, _ := url.Parse(loginW.Header().Get("Location"))
	state := u.Query().Get("state")
	var stateCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == auth.StateCookie {
			stateCookie = c
		}
	}
	require.NotNil(t, stateCookie)

	// Step 2: /auth/callback with the matching state + code.
	cbReq := httptest.NewRequest("GET", "/auth/callback?code=good&state="+state, nil)
	cbReq.AddCookie(stateCookie)
	cbW := httptest.NewRecorder()
	router.ServeHTTP(cbW, cbReq)
	require.Equal(t, http.StatusFound, cbW.Code)

	var sessionCookie *http.Cookie
	for _, c := range cbW.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie, "callback must set a session cookie")

	// Step 3: the session identifies the stubbed login (PazerOP, an admin).
	meReq := httptest.NewRequest("GET", "/api/me", nil)
	meReq.AddCookie(sessionCookie)
	meW := httptest.NewRecorder()
	router.ServeHTTP(meW, meReq)
	require.Equal(t, http.StatusOK, meW.Code)
	var me meResponse
	require.NoError(t, json.Unmarshal(meW.Body.Bytes(), &me))
	assert.True(t, me.Authenticated)
	assert.Equal(t, "PazerOP", me.Login)
	assert.True(t, me.IsAdmin)
}

func TestDashboard_Callback_BadState(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/auth/callback?code=good&state=wrong", nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: "different"})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDashboard_ServeIndex(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "GitHub State Mirror")
}

func TestDashboard_ServeAssets(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))

	js := httptest.NewRequest("GET", "/assets/app.js", nil)
	jsW := httptest.NewRecorder()
	router.ServeHTTP(jsW, js)
	require.Equal(t, http.StatusOK, jsW.Code)
	assert.Contains(t, jsW.Header().Get("Content-Type"), "javascript")

	css := httptest.NewRequest("GET", "/assets/style.css", nil)
	cssW := httptest.NewRecorder()
	router.ServeHTTP(cssW, css)
	require.Equal(t, http.StatusOK, cssW.Code)
	assert.Contains(t, cssW.Header().Get("Content-Type"), "css")

	// demo-data.js is preview-only and must NOT be embedded/served.
	demo := httptest.NewRequest("GET", "/assets/demo-data.js", nil)
	demoW := httptest.NewRecorder()
	router.ServeHTTP(demoW, demo)
	assert.Equal(t, http.StatusNotFound, demoW.Code)
}

func TestGroupKinds(t *testing.T) {
	rows := []dbgen.ActorFreshnessByKindRow{
		{ResourceKind: "org_repos", FetchState: "fresh", Count: 2, LastFetched: "2026-01-01T00:00:00Z"},
		{ResourceKind: "org_repos", FetchState: "stale", Count: 1, LastFetched: "2026-02-01T00:00:00Z"},
		{ResourceKind: "org_repos", FetchState: "error", Count: 1, LastFetched: "2026-02-02T00:00:00Z"},
		{ResourceKind: "pr_files", FetchState: "fresh", Count: 5, LastFetched: []byte("2026-03-01T00:00:00Z")},
	}
	errRows := []dbgen.ActorErrorMessagesByKindRow{
		{ResourceKind: "org_repos", ResourceKey: "wow-look-at-my", ErrorMessage: sql.NullString{String: "github api POST /graphql: 502", Valid: true}},
	}
	out := groupKinds(rows, errRows)
	require.Len(t, out, 2)
	assert.Equal(t, "org_repos", out[0].Kind)
	assert.Equal(t, int64(2), out[0].States["fresh"])
	assert.Equal(t, int64(1), out[0].States["stale"])
	assert.Equal(t, "2026-02-02T00:00:00Z", out[0].LastFetched) // max across states
	assert.Equal(t, "github api POST /graphql: 502", out[0].Error)
	assert.Equal(t, "wow-look-at-my", out[0].ErrorKey)
	assert.Equal(t, "2026-03-01T00:00:00Z", out[1].LastFetched) // []byte coerced
	assert.Empty(t, out[1].Error)
}

func TestSumAndShortAndTime(t *testing.T) {
	c := ghdata.DataCounts{Repos: 1, PullRequests: 2, Orgs: 3, Users: 4, CommitChecks: 5, PRFiles: 6, BranchComparisons: 7}
	assert.Equal(t, int64(28), sumCounts(c))

	assert.Equal(t, "0123456789ab", shortFingerprint("0123456789abcdef"))
	assert.Equal(t, "short", shortFingerprint("short"))

	assert.Equal(t, "x", asTimeString("x"))
	assert.Equal(t, "y", asTimeString([]byte("y")))
	assert.Equal(t, "", asTimeString(nil))
	assert.Equal(t, "", asTimeString(42))
}

// TestToRecent_SurfacesError verifies a failed refresh carries its captured
// error_message through to the dashboard response, while a successful one does
// not — so the UI can show *why* a refresh errored, not just that it did.
func TestToRecent_SurfacesError(t *testing.T) {
	logs := []dbgen.CacheRefreshLog{
		{
			ResourceKind: "org_repos", ResourceKey: "wow-look-at-my", TriggeredBy: "lazy",
			StartedAt:    "2024-01-01T00:00:00Z",
			CompletedAt:  sql.NullString{String: "2024-01-01T00:00:01Z", Valid: true},
			Success:      sql.NullInt64{Int64: 0, Valid: true},
			ErrorMessage: sql.NullString{String: "github graphql: 403 Forbidden", Valid: true},
		},
		{
			ResourceKind: "user", ResourceKey: "self", TriggeredBy: "lazy",
			StartedAt:   "2024-01-01T00:00:00Z",
			CompletedAt: sql.NullString{String: "2024-01-01T00:00:01Z", Valid: true},
			Success:     sql.NullInt64{Int64: 1, Valid: true},
		},
	}
	out := toRecent(logs)
	require.Len(t, out, 2)

	assert.Equal(t, "error", out[0].Status)
	assert.Equal(t, "github graphql: 403 Forbidden", out[0].Error, "errored refresh must carry its failure reason")

	assert.Equal(t, "success", out[1].Status)
	assert.Empty(t, out[1].Error, "successful refresh has no error detail")
}
