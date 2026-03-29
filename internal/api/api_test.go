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
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
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
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	// Register stub fetchers so EnsureFresh doesn't panic.
	for _, kind := range []string{syncpkg.KindUser, syncpkg.KindUserOrgs, syncpkg.KindOrgRepos, syncpkg.KindPRFiles, syncpkg.KindCompare} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	dispatcher := syncpkg.NewWebhookDispatcher(mgr)
	router := NewRouter(mgr, store, "", dispatcher)
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["login"] != "octocat" {
		t.Errorf("login = %v, want %q", body["login"], "octocat")
	}
}

func TestGetUser_NotFound(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1", len(body))
	}
	if body[0]["login"] != "org1" {
		t.Errorf("login = %v, want %q", body[0]["login"], "org1")
	}
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

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1", len(body))
	}
	if body[0]["filename"] != "main.go" {
		t.Errorf("filename = %v, want %q", body[0]["filename"], "main.go")
	}
}

func TestGetCompare(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := context.Background()

	store.UpsertComparison(ctx, dbgen.BranchComparison{
		Owner: "org1", Repo: "repo1", BaseRef: "main", HeadRef: "feature", AheadBy: 5, BehindBy: 2,
	})

	req := httptest.NewRequest("GET", "/repos/org1/repo1/compare/main...feature", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ahead_by"] != float64(5) {
		t.Errorf("ahead_by = %v, want 5", body["ahead_by"])
	}
	if body["behind_by"] != float64(2) {
		t.Errorf("behind_by = %v, want 2", body["behind_by"])
	}
}
