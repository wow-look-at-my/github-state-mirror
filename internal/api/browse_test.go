package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// seedBrowseTruth fills global truth with one row in each table (plus a grant
// for a principal) so the browse endpoints exercise every converter.
func seedBrowseTruth(t *testing.T, store *ghdata.Store, principal, login string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	require.NoError(t, store.RecordActorIdentity(ctx, principal, login))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		Visibility:          ghdata.VisibilityPrivate,
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "SUCCESS", Valid: true},
	}))
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7, Title: "A PR", Url: "https://github.com/org1/repo1/pull/7",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
		LastCommitStatus: sql.NullString{String: "SUCCESS", Valid: true},
	}, now))
	require.NoError(t, store.SetPRLabels(ctx, "org1", "repo1", 7, []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 7, Name: "bug", Color: "d73a4a"},
	}))
	_, err := store.ApplyCommitStatus(ctx, "org1", "repo1", "sha1", "ci/build", "SUCCESS", false)
	require.NoError(t, err)
	require.NoError(t, store.RecordGrant(ctx, principal, "org1", "repo1", ghdata.GrantSourceListSync, now))
}

func TestBrowse_AdminSeesGlobalTruth(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	seedBrowseTruth(t, store, "user:900", "octocat")

	req := httptest.NewRequest("GET", "/api/cache/data", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body browseResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	require.Len(t, body.Repos, 1)
	assert.Equal(t, "SUCCESS", body.Repos[0].DefaultBranchStatus)
	assert.Equal(t, "main", body.Repos[0].DefaultBranch)
	assert.Equal(t, ghdata.VisibilityPrivate, body.Repos[0].Visibility)
	require.Len(t, body.PullRequests, 1)
	assert.Equal(t, int64(7), body.PullRequests[0].Number)
	assert.Equal(t, []string{"bug"}, body.PullRequests[0].Labels)
	assert.Equal(t, "SUCCESS", body.PullRequests[0].LastCommitStatus)
	assert.False(t, body.PullRequests[0].RestComplete, "a bare-seeded row lacks REST-only fields")
	require.Len(t, body.CommitChecks, 1)
	assert.Equal(t, "ci/build", body.CommitChecks[0].Context)
	assert.Equal(t, int64(1), body.Counts.Repos)
	assert.Equal(t, int64(1), body.Counts.Grants)
}

func TestBrowse_GrantsView(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	seedBrowseTruth(t, store, "user:900", "octocat")

	req := httptest.NewRequest("GET", "/api/cache/data?principal=user:900", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body grantsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, "user:900", body.PrincipalID)
	assert.Equal(t, "octocat", body.Login)
	require.Len(t, body.Grants, 1)
	assert.Equal(t, "org1", body.Grants[0].Owner)
	assert.Equal(t, "repo1", body.Grants[0].Repo)
	assert.Equal(t, ghdata.GrantSourceListSync, body.Grants[0].Source)
}

func TestBrowse_Unauthenticated(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/api/cache/data", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBrowse_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/data", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not in AdminLogins
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCacheCheck_UnavailableWithoutApp(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc) // test stack wires a checker with a nil app
	req := httptest.NewRequest("GET", "/api/cache/check", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCacheCheck_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/check", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestCacheCheckApply_NonAdminForbidden: the write surface is admin-gated
// exactly like the read.
func TestCacheCheckApply_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("POST", "/api/cache/check?apply=true", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestCacheCheckApply_RequiresPost: apply on a GET is refused, so a
// prefetched or bookmarked URL can never mutate the cache.
func TestCacheCheckApply_RequiresPost(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/check?apply=1", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// TestCacheCheckApply_UnavailableWithoutApp: like the read, apply needs the
// GitHub App.
func TestCacheCheckApply_UnavailableWithoutApp(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("POST", "/api/cache/check?apply=true", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestRateLimit_NoAppStillServesObserved: without a GitHub App the endpoint no
// longer 503s — it answers 200 with an empty live half, an explanatory note,
// and whatever the passive meter observed (nothing yet, here).
func TestRateLimit_NoAppStillServesObserved(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc) // test stack wires a checker with a nil app

	req := httptest.NewRequest("GET", "/api/ratelimit", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body rateLimitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Live)
	assert.NotNil(t, body.Observed)
	assert.Empty(t, body.Observed)
	assert.Contains(t, body.Note, "no GitHub App configured")
}

// TestRateLimit_ObservesPassthroughHeaders drives a passthrough request whose
// upstream response carries X-RateLimit-* headers and asserts the reading
// surfaces on /api/ratelimit — the end-to-end passive-observation path.
func TestRateLimit_ObservesPassthroughHeaders(t *testing.T) {
	svc := configuredAuth(t)
	// The reset must be in the FUTURE: the ratemeter lazily prunes
	// observations whose reset moment has passed, and this test exercises the
	// observation path, not the aging rule.
	reset := time.Now().Add(time.Hour).Unix()
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Used", "679")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.Header().Set("X-RateLimit-Resource", "core")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	router, _, _, _ := newTestStackWithGitHub(t, svc, gh)

	// An unknown route falls through to the passthrough proxy (no requireAuth,
	// so the identity is the token-fingerprint label).
	req := httptest.NewRequest("GET", "/meta", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest("GET", "/api/ratelimit", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body rateLimitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Observed, 1)
	o := body.Observed[0]
	assert.Equal(t, "token:"+ghclient.Fingerprint(testToken)[:12], o.Identity)
	assert.Equal(t, "", o.Name, "a token-fingerprint identity has no verified name")
	assert.Equal(t, "core", o.Resource)
	assert.Equal(t, 5000, o.Limit)
	assert.Equal(t, 4321, o.Remaining)
	assert.Equal(t, 679, o.Used)
	assert.Equal(t, reset, o.Reset)
	assert.NotEmpty(t, o.ObservedAt)
	_, err := time.Parse(time.RFC3339, o.ObservedAt)
	assert.NoError(t, err, "observed_at must be RFC3339")
}

// TestRateLimit_ObservedGroupsByPrincipal: a cached-route miss fetch runs
// inside requireAuth, so its observation is recorded under the resolved
// principal (user:<id>) rather than a token fingerprint.
func TestRateLimit_ObservedGroupsByPrincipal(t *testing.T) {
	svc := configuredAuth(t)
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
			return
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, svc, gh)

	// A cached route: the reveal probe + miss fetch both hit the fake GitHub
	// with the requireAuth principal in context.
	req := httptest.NewRequest("GET", "/repos/org1/repo1/contents/README.md", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	req = httptest.NewRequest("GET", "/api/ratelimit", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body rateLimitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Observed)
	assert.Equal(t, testUserActor, body.Observed[0].Identity)
	assert.Equal(t, testUserLogin, body.Observed[0].Name,
		"the requireAuth-resolved login rides the observation as its display name")
}

func TestRateLimit_Unauthenticated(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/api/ratelimit", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRateLimit_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/ratelimit", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not in AdminLogins
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
