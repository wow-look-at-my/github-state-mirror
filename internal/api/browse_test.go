package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// seedBrowseScope fills a scope with one row in each cached table so the browse
// endpoint exercises every converter.
func seedBrowseScope(t *testing.T, store *ghdata.Store, fp, login string) {
	t.Helper()
	ctx := actor.WithActor(context.Background(), fp)
	require.NoError(t, store.RecordActorIdentity(context.Background(), fp, login))
	require.NoError(t, store.UpsertUser(ctx, dbgen.User{Login: login, AvatarUrl: "a", Url: "https://u"}))
	require.NoError(t, store.UpsertOrg(ctx, dbgen.Org{Login: "org1", Url: sql.NullString{String: "https://org1", Valid: true}}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "SUCCESS", Valid: true},
	}))
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7, Title: "A PR", Url: "https://github.com/org1/repo1/pull/7",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
		LastCommitStatus: sql.NullString{String: "SUCCESS", Valid: true},
	}))
	require.NoError(t, store.SetPRLabels(ctx, "org1", "repo1", 7, []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 7, Name: "bug", Color: "d73a4a"},
	}))
	require.NoError(t, store.SetPRFiles(ctx, "org1", "repo1", 7, []dbgen.PrFile{
		{Owner: "org1", Repo: "repo1", PrNumber: 7, Path: "main.go", Additions: 3, Deletions: 1},
	}))
	require.NoError(t, store.UpsertComparison(ctx, dbgen.BranchComparison{
		Owner: "org1", Repo: "repo1", BaseRef: "main", HeadRef: "feature", AheadBy: 2, BehindBy: 1,
	}))
	_, err := store.ApplyCommitStatusForActors(context.Background(), []string{fp}, "org1", "repo1", "sha1", "ci/build", "SUCCESS", false)
	require.NoError(t, err)
}

func TestBrowse_AdminSeesData(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	fp := "browse-fp"
	seedBrowseScope(t, store, fp, "octocat")

	req := httptest.NewRequest("GET", "/api/cache/data?actor="+fp, nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body browseResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, fp, body.ActorID)
	assert.Equal(t, "octocat", body.Login)
	require.Len(t, body.Repos, 1)
	assert.Equal(t, "SUCCESS", body.Repos[0].DefaultBranchStatus)
	assert.Equal(t, "main", body.Repos[0].DefaultBranch)
	require.Len(t, body.PullRequests, 1)
	assert.Equal(t, int64(7), body.PullRequests[0].Number)
	assert.Equal(t, []string{"bug"}, body.PullRequests[0].Labels)
	assert.Equal(t, "SUCCESS", body.PullRequests[0].LastCommitStatus)
	require.Len(t, body.Orgs, 1)
	require.Len(t, body.Users, 1)
	require.Len(t, body.PRFiles, 1)
	require.Len(t, body.BranchComparisons, 1)
	require.Len(t, body.CommitChecks, 1)
	assert.Equal(t, "ci/build", body.CommitChecks[0].Context)
	assert.Equal(t, int64(1), body.Counts.Repos)
}

func TestBrowse_MissingActor(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/data", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBrowse_Unauthenticated(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))
	req := httptest.NewRequest("GET", "/api/cache/data?actor=x", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBrowse_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/data?actor=x", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not in AdminLogins
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCacheCheck_UnavailableWithoutApp(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc) // test stack wires a checker with a nil app
	req := httptest.NewRequest("GET", "/api/cache/check?actor=x", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCacheCheck_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/check?actor=x", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
