package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

// stubFetcher always succeeds (used to satisfy EnsureFresh without hitting GitHub).
type stubFetcher struct{}

func (f *stubFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	return freshness.RefreshResult{}, nil
}

func setupTestRouter(t *testing.T) (http.Handler, *ghdata.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	// Register stub fetchers so EnsureFresh doesn't panic.
	for _, kind := range []string{syncpkg.KindUser, syncpkg.KindUserOrgs, syncpkg.KindOrgRepos, syncpkg.KindPRFiles, syncpkg.KindCompare} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)
	gh := ghclient.New("")
	router := NewRouter(mgr, store, "", dispatcher, gh)
	return router, store
}

func TestGetUser(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := context.Background()

	// Seed data.
	store.UpsertUser(ctx, dbgen.User{Login: "octocat", AvatarUrl: "http://avatar", Url: "http://url"})

	req := httptest.NewRequest("GET", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, "octocat", body["login"])

}

func TestGetUser_NotFound(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

}

func TestGetUserOrgs(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := context.Background()

	store.UpsertUser(ctx, dbgen.User{Login: "octocat", AvatarUrl: "a", Url: "u"})
	store.SetUserOrgs(ctx, "octocat", []dbgen.Org{
		{Login: "org1", AvatarUrl: sql.NullString{String: "a1", Valid: true}, Url: sql.NullString{String: "u1", Valid: true}},
	})

	req := httptest.NewRequest("GET", "/user/orgs", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	require.Equal(t, 1, len(body))

	assert.Equal(t, "org1", body[0]["login"])

}

func TestGetPRFiles(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := context.Background()

	store.SetPRFiles(ctx, "org1", "repo1", 42, []dbgen.PrFile{
		{Owner: "org1", Repo: "repo1", PrNumber: 42, Path: "main.go", Additions: 10, Deletions: 5},
	})

	req := httptest.NewRequest("GET", "/repos/org1/repo1/pulls/42/files", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	require.Equal(t, 1, len(body))

	assert.Equal(t, "main.go", body[0]["filename"])

}

func TestGetCompare(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := context.Background()

	store.UpsertComparison(ctx, dbgen.BranchComparison{
		Owner:	"org1", Repo: "repo1", BaseRef: "main", HeadRef: "feature", AheadBy: 5, BehindBy: 2,
	})

	req := httptest.NewRequest("GET", "/repos/org1/repo1/compare/main...feature", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, float64(5), body["ahead_by"])

	assert.Equal(t, float64(2), body["behind_by"])

}
