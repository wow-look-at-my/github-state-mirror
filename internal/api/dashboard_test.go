package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// seedPrincipal populates a principal with an identity mapping, a grant into
// global truth, and a sync-marker freshness row.
func seedPrincipal(t *testing.T, store *ghdata.Store, db *sql.DB, principal, login string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "o", Name: "r", NameWithOwner: "o/r", Url: "u"}))
	require.NoError(t, store.RecordGrant(ctx, principal, "o", "r", ghdata.GrantSourceListSync, time.Now()))
	require.NoError(t, store.RecordActorIdentity(ctx, principal, login))
	seedFreshness(t, db, principal, "org_repos", "o")
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
	seedPrincipal(t, store, db, "user:100", "octocat")

	req := httptest.NewRequest("GET", "/api/cache?scope=mine", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp cacheResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "octocat", resp.Login)
	assert.Equal(t, "mine", resp.Scope)
	assert.Equal(t, 1, resp.PrincipalCount)
	require.Len(t, resp.Principals, 1)
	assert.Equal(t, "octocat", resp.Principals[0].Login)
	assert.True(t, resp.Principals[0].IsSelf)
	assert.Equal(t, int64(1), resp.Principals[0].LiveGrants)
	assert.Equal(t, int64(1), resp.Totals.Repos, "totals are the GLOBAL truth counts")
	assert.Equal(t, int64(1), resp.Totals.Grants)
	// org_repos sync-marker freshness should surface.
	require.NotEmpty(t, resp.Principals[0].Kinds)
	assert.Equal(t, "org_repos", resp.Principals[0].Kinds[0].Kind)
	assert.Equal(t, int64(1), resp.Principals[0].Kinds[0].States["fresh"])
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
	assert.Equal(t, 0, resp.PrincipalCount)
	assert.Equal(t, []principalStats{}, resp.Principals)
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
	seedPrincipal(t, store, db, "user:100", "octocat")

	req := httptest.NewRequest("GET", "/api/cache?scope=all", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_CacheAll_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedPrincipal(t, store, db, "user:100", "octocat")
	seedPrincipal(t, store, db, "user:200", "PazerOP")
	// An orphan principal: sync markers but no identity row.
	seedFreshness(t, db, "app-installation:9", "org_repos", "other-org")

	req := httptest.NewRequest("GET", "/api/cache?scope=all", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp cacheResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "all", resp.Scope)
	assert.Equal(t, 3, resp.PrincipalCount)

	byLogin := map[string]principalStats{}
	for _, s := range resp.Principals {
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
}

func TestDashboard_Webhooks_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedPrincipal(t, store, db, "user:100", "octocat")

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
	// An expired/foreign state gets the retry page, not a bare text dead end.
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), `href="/login"`, "failure page must link the login start")
}

// brokenAuth returns an auth.Service whose stub GitHub fails the requested leg
// of the callback flow — the code-for-token exchange ("token") or the identity
// read ("user") — while the other leg succeeds, so the failure lands exactly on
// the branch under test.
func brokenAuth(t *testing.T, failing string) *auth.Service {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		if failing == "token" {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_x"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		if failing == "user" {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "PazerOP", "avatar_url": "a"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return auth.New(auth.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		SessionKey:   []byte("test-session-key"),
		TokenURL:     srv.URL + "/token",
		APIBaseURL:   srv.URL,
	})
}

// callbackWithFreshState runs /login to mint a state + its cookie, then hits
// /auth/callback with the matching pair, returning the callback's recorder.
func callbackWithFreshState(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	loginReq := httptest.NewRequest("GET", "/login", nil)
	loginW := httptest.NewRecorder()
	router.ServeHTTP(loginW, loginReq)
	require.Equal(t, http.StatusFound, loginW.Code)
	u, err := url.Parse(loginW.Header().Get("Location"))
	require.NoError(t, err)
	state := u.Query().Get("state")
	var stateCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == auth.StateCookie {
			stateCookie = c
		}
	}
	require.NotNil(t, stateCookie)

	cbReq := httptest.NewRequest("GET", "/auth/callback?code=good&state="+state, nil)
	cbReq.AddCookie(stateCookie)
	cbW := httptest.NewRecorder()
	router.ServeHTTP(cbW, cbReq)
	return cbW
}

// TestDashboard_Callback_ExchangeFailure pins the 2026-07-19 incident fix: a
// failed code-for-token exchange must answer a 4xx retry page, never a 5xx —
// Cloudflare replaces origin 5xx bodies with its own bare error page, stranding
// the user with zero context and no way to retry.
func TestDashboard_Callback_ExchangeFailure(t *testing.T) {
	router, _, _ := newTestStack(t, brokenAuth(t, "token"))
	w := callbackWithFreshState(t, router)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), `href="/login"`, "failure page must link the login start")
	for _, c := range w.Result().Cookies() {
		assert.NotEqual(t, auth.SessionCookie, c.Name, "a failed callback must not set a session")
	}
}

// TestDashboard_Callback_UserFetchFailure is the same pin for the follow-up
// GET /user leg: identity-read failures render the 4xx retry page too.
func TestDashboard_Callback_UserFetchFailure(t *testing.T) {
	router, _, _ := newTestStack(t, brokenAuth(t, "user"))
	w := callbackWithFreshState(t, router)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), `href="/login"`, "failure page must link the login start")
	for _, c := range w.Result().Cookies() {
		assert.NotEqual(t, auth.SessionCookie, c.Name, "a failed callback must not set a session")
	}
}

// TestDashboard_Callback_MissingCode: a stateful callback without a code is
// logged and gets the same retry page.
func TestDashboard_Callback_MissingCode(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	loginReq := httptest.NewRequest("GET", "/login", nil)
	loginW := httptest.NewRecorder()
	router.ServeHTTP(loginW, loginReq)
	require.Equal(t, http.StatusFound, loginW.Code)
	u, err := url.Parse(loginW.Header().Get("Location"))
	require.NoError(t, err)
	state := u.Query().Get("state")
	var stateCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == auth.StateCookie {
			stateCookie = c
		}
	}
	require.NotNil(t, stateCookie)

	req := httptest.NewRequest("GET", "/auth/callback?state="+state, nil)
	req.AddCookie(stateCookie)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `href="/login"`, "failure page must link the login start")
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

	// Every embedded asset is also reachable at its stable name (the fallback
	// path the backend-free preview relies on).
	for path, wantType := range map[string]string{
		"/assets/app.js":        "javascript",
		"/assets/rate-meter.js": "javascript",
		"/assets/style.css":     "css",
	} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, path)
		assert.Contains(t, w.Header().Get("Content-Type"), wantType, path)
	}

	// demo-data.js is preview-only and must NOT be embedded/served.
	demo := httptest.NewRequest("GET", "/assets/demo-data.js", nil)
	demoW := httptest.NewRecorder()
	router.ServeHTTP(demoW, demo)
	assert.Equal(t, http.StatusNotFound, demoW.Code)
}

// TestDashboard_HashedAssets verifies the served index references the
// content-addressed asset URLs (never the stable names) for every embedded
// asset, and that those hashed URLs are served with the immutable cache header.
func TestDashboard_HashedAssets(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))

	idx := httptest.NewRequest("GET", "/", nil)
	idxW := httptest.NewRecorder()
	router.ServeHTTP(idxW, idx)
	require.Equal(t, http.StatusOK, idxW.Code)
	html := idxW.Body.String()

	for _, a := range []struct{ stem, ext string }{
		{"app", "js"},
		{"rate-meter", "js"},
		{"style", "css"},
	} {
		re := regexp.MustCompile(`assets/` + regexp.QuoteMeta(a.stem) + `\.[0-9a-f]{10}\.` + a.ext)
		hashed := re.FindString(html)
		require.NotEmpty(t, hashed, "index must reference a hashed URL for %s.%s", a.stem, a.ext)
		assert.NotContains(t, html, `"assets/`+a.stem+`.`+a.ext+`"`, "stable name must be rewritten")

		req := httptest.NewRequest("GET", "/"+hashed, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, hashed)
		assert.Contains(t, w.Header().Get("Cache-Control"), "immutable", hashed)
	}
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

func TestShortAndTime(t *testing.T) {
	assert.Equal(t, "0123456789ab", shortFingerprint("0123456789abcdef"))
	assert.Equal(t, "short", shortFingerprint("short"))
	// Structured actors are never truncated — cutting "user:12345678901" at 12
	// chars would drop significant id digits.
	assert.Equal(t, "user:12345678901", shortFingerprint("user:12345678901"))
	assert.Equal(t, "app-installation:123", shortFingerprint("app-installation:123"))
	assert.Equal(t, "app:99", shortFingerprint("app:99"))

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

// seedJob writes one workflow job row (global — no actor scoping).
func seedJob(t *testing.T, store *ghdata.Store, id int64, name, status, conclusion, startedAt, completedAt string) {
	t.Helper()
	require.NoError(t, store.RecordWorkflowJob(context.Background(), ghdata.WorkflowJob{
		Owner: "o", Repo: "r", JobID: id, RunID: 5, RunAttempt: 1,
		Name: name, WorkflowName: "CI", Status: status, Conclusion: conclusion,
		HeadSHA: "cafe", HeadBranch: "main", HTMLURL: "https://github.com/o/r/actions/runs/5/job/1",
		StartedAt: startedAt, CompletedAt: completedAt,
	}))
}

// jobTime renders a fixture timestamp N hours in the past — RELATIVE to now,
// because workflow jobs completed more than workflowJobRetention (14d) ago are
// pruned on write: hardcoded dates rotted out of the window and started
// failing these tests on 2026-07-15.
func jobTime(hoursAgo int) string {
	return time.Now().Add(-time.Duration(hoursAgo) * time.Hour).UTC().Format(time.RFC3339)
}

func TestDashboard_Jobs_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	seedJob(t, store, 1, "done-old", "completed", "success", jobTime(73), jobTime(72))
	seedJob(t, store, 2, "done-new", "completed", "failure", jobTime(49), jobTime(48))
	seedJob(t, store, 3, "running", "in_progress", "", jobTime(24), "")

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp jobsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Jobs, 3)
	// Running first, then completed newest-completed first.
	assert.Equal(t, "running", resp.Jobs[0].Name)
	assert.Equal(t, "done-new", resp.Jobs[1].Name)
	assert.Equal(t, "done-old", resp.Jobs[2].Name)
	assert.Equal(t, "failure", resp.Jobs[1].Conclusion)
	assert.Equal(t, int64(2), resp.Jobs[1].JobID)
	assert.Equal(t, "o", resp.Jobs[0].Owner)
	assert.Equal(t, "r", resp.Jobs[0].Repo)
}

func TestDashboard_Jobs_Limit(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	seedJob(t, store, 1, "a", "completed", "success", jobTime(73), jobTime(72))
	seedJob(t, store, 2, "b", "completed", "success", jobTime(49), jobTime(48))
	seedJob(t, store, 3, "c", "in_progress", "", jobTime(24), "")

	// limit honored.
	req := httptest.NewRequest("GET", "/api/jobs?limit=1", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var resp jobsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Jobs, 1)
	assert.Equal(t, "c", resp.Jobs[0].Name)

	// A limit beyond the cap is clamped (still 200; returns what exists).
	req = httptest.NewRequest("GET", "/api/jobs?limit=99999", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	resp = jobsResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Jobs, 3)

	// Garbage limit is a 400.
	req = httptest.NewRequest("GET", "/api/jobs?limit=zero", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestDashboard_Jobs_LimitCapEnforced seeds more rows than the cap and
// verifies the response is clamped to jobsMaxLimit even when a larger limit is
// requested.
func TestDashboard_Jobs_LimitCapEnforced(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	for i := 1; i <= jobsMaxLimit+10; i++ {
		seedJob(t, store, int64(i), "j", "in_progress", "", "2026-07-03T10:00:00Z", "")
	}

	req := httptest.NewRequest("GET", "/api/jobs?limit=10000", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp jobsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Jobs, jobsMaxLimit)
}

func TestDashboard_Jobs_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedPrincipal(t, store, db, "user:100", "octocat")

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_Jobs_Unauthenticated(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
