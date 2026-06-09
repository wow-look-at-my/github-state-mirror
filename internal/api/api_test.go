package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testToken is the bearer token sent by authenticated test requests.
const testToken = "test-token"

// testWebhookSecret is the HMAC secret the test router verifies webhooks against.
const testWebhookSecret = "test-webhook-secret"

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

	// Stub GitHub's /user endpoint so requireAuth can validate the test token
	// without reaching the real API.
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
	}))
	t.Cleanup(ghSrv.Close)
	gh := ghclient.NewWithBaseURL("", ghSrv.URL)

	router := NewRouter(mgr, store, testWebhookSecret, dispatcher, gh, []string{"*"})
	return router, store
}

// sign returns the GitHub HMAC signature header value for a webhook body.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// seedCtx returns a context scoped to the same cache partition that an
// authenticated request bearing testToken resolves to.
func seedCtx() context.Context {
	return actor.WithActor(context.Background(), ghclient.Fingerprint(testToken))
}

// authedReq builds a request carrying a valid bearer token.
func authedReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

func TestGetUser(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := seedCtx()

	// Seed data.
	store.UpsertUser(ctx, dbgen.User{Login: "octocat", AvatarUrl: "http://avatar", Url: "http://url"})

	req := authedReq("GET", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, "octocat", body["login"])

}

func TestGetUser_NotFound(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := authedReq("GET", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

}

func TestGetUserOrgs(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := seedCtx()

	store.UpsertUser(ctx, dbgen.User{Login: "octocat", AvatarUrl: "a", Url: "u"})
	store.SetUserOrgs(ctx, "octocat", []dbgen.Org{
		{Login: "org1", AvatarUrl: sql.NullString{String: "a1", Valid: true}, Url: sql.NullString{String: "u1", Valid: true}},
	})

	req := authedReq("GET", "/user/orgs", nil)
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
	ctx := seedCtx()

	store.SetPRFiles(ctx, "org1", "repo1", 42, []dbgen.PrFile{
		{Owner: "org1", Repo: "repo1", PrNumber: 42, Path: "main.go", Additions: 10, Deletions: 5},
	})

	req := authedReq("GET", "/repos/org1/repo1/pulls/42/files", nil)
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
	ctx := seedCtx()

	store.UpsertComparison(ctx, dbgen.BranchComparison{
		Owner:	"org1", Repo: "repo1", BaseRef: "main", HeadRef: "feature", AheadBy: 5, BehindBy: 2,
	})

	req := authedReq("GET", "/repos/org1/repo1/compare/main...feature", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, float64(5), body["ahead_by"])

	assert.Equal(t, float64(2), body["behind_by"])

}

// TestRequireAuth_Unauthenticated verifies that data endpoints reject requests
// with no Authorization header instead of silently serving the server's
// GITHUB_TOKEN view.
func TestRequireAuth_Unauthenticated(t *testing.T) {
	router, store := setupTestRouter(t)

	// Seed data in a credential bucket; an unauthenticated caller must not see it.
	store.UpsertUser(seedCtx(), dbgen.User{Login: "octocat", AvatarUrl: "a", Url: "u"})

	for _, target := range []string{
		"/user",
		"/user/orgs",
		"/repos/org1/repo1/pulls/42/files",
		"/repos/org1/repo1/compare/main...feature",
	} {
		req := httptest.NewRequest("GET", target, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "GET %s without a token must be 401", target)
	}

	// GraphQL too.
	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"x","variables":{"org":"my-org"}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestCredentialIsolation verifies that data cached under one credential is not
// served to a request bearing a different credential, even for the same login.
func TestCredentialIsolation(t *testing.T) {
	router, store := setupTestRouter(t)

	// Seed PR files visible only to testToken's bucket.
	store.SetPRFiles(seedCtx(), "org1", "repo1", 42, []dbgen.PrFile{
		{Owner: "org1", Repo: "repo1", PrNumber: 42, Path: "secret.go", Additions: 1, Deletions: 0},
	})

	// A different token (same stubbed login) resolves to a different bucket and
	// must see nothing.
	req := httptest.NewRequest("GET", "/repos/org1/repo1/pulls/42/files", nil)
	req.Header.Set("Authorization", "Bearer other-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, 0, len(body), "a different credential must not see another credential's cached files")
}

// TestWebhook_NoAuthRequired verifies the webhook endpoint is reachable without
// a bearer token (it is authenticated by HMAC signature instead).
func TestWebhook_NoAuthRequired(t *testing.T) {
	router, _ := setupTestRouter(t)

	body := "{}"
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, []byte(body)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
