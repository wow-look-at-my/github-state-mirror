package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
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
