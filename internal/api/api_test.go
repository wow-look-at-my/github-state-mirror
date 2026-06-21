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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
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

// newTestStack builds the full router with the given auth service, returning the
// router, the data store, and the underlying DB (for seeding freshness rows).
// Its fake GitHub answers every path with a login JSON, enough for requireAuth.
func newTestStack(t *testing.T, authSvc *auth.Service) (http.Handler, *ghdata.Store, *sql.DB) {
	t.Helper()
	// Stub GitHub's /user endpoint so requireAuth can validate the test token
	// without reaching the real API.
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
	})
	router, store, db, _ := newTestStackWithGitHub(t, authSvc, gh)
	return router, store, db
}

// newTestStackWithGitHub is like newTestStack but lets the caller supply the
// fake upstream GitHub handler, and returns its URL — used by passthrough tests
// that need to observe forwarded requests. requireAuth resolves the bearer
// token against this same handler, so it must answer GET /user with a login.
func newTestStackWithGitHub(t *testing.T, authSvc *auth.Service, ghHandler http.Handler) (http.Handler, *ghdata.Store, *sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	// Register stub fetchers so EnsureFresh doesn't panic.
	for _, kind := range []string{syncpkg.KindUser, syncpkg.KindUserOrgs, syncpkg.KindOrgRepos, syncpkg.KindPullRequestRaw, syncpkg.KindRepoPullList, syncpkg.KindRepoContents, syncpkg.KindPRFiles, syncpkg.KindCompare} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store, nil)

	ghSrv := httptest.NewServer(ghHandler)
	t.Cleanup(ghSrv.Close)
	gh := ghclient.NewWithBaseURL(ghSrv.URL)
	mgr.RegisterFetcher(freshness.Policy{
		Kind:          syncpkg.KindPullRequestRaw,
		DefaultTTL:    30 * time.Minute,
		ErrorRetryMin: 30 * time.Second,
	}, syncpkg.NewPullRequestRawFetcher(gh, store))
	mgr.RegisterFetcher(freshness.Policy{
		Kind:          syncpkg.KindRepoContents,
		DefaultTTL:    30 * time.Minute,
		ErrorRetryMin: 30 * time.Second,
	}, syncpkg.NewRepoContentsFetcher(gh, store))
	mgr.RegisterFetcher(freshness.Policy{
		Kind:          syncpkg.KindRepoPullList,
		DefaultTTL:    30 * time.Minute,
		ErrorRetryMin: 30 * time.Second,
	}, syncpkg.NewRepoPullListFetcher(gh, store))

	// nil app -> the consistency checker reports Available()==false, the realistic
	// "no GitHub App configured" state for these tests.
	checker := syncpkg.NewConsistencyChecker(gh, store, nil)
	router := NewRouter(mgr, store, testWebhookSecret, dispatcher, gh, []string{"*"}, authSvc, "", checker)
	return router, store, db, ghSrv.URL
}

func setupTestRouter(t *testing.T) (http.Handler, *ghdata.Store) {
	router, store, _ := newTestStack(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}))
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

// NOTE: the /user, /user/orgs, /compare, and /pulls/{n}/files cached endpoints
// were removed (they returned trimmed, non-GitHub shapes). They now pass through
// through GitHub uncached — see TestProxy_FormerlyCachedNowForwarded in proxy_test.go.
// The only cached data route left is POST /graphql (the org-repos query).

// TestRequireAuth_Unauthenticated verifies that data endpoints reject requests
// with no Authorization header. The formerly-cached REST paths now fall through
// to the passthrough proxy, which itself enforces the token; the GraphQL route
// is gated by requireAuth. Either way a tokenless request is 401.
func TestRequireAuth_Unauthenticated(t *testing.T) {
	router, _ := setupTestRouter(t)

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

// TestCredentialIsolation verifies that data cached under one credential (the
// org-repos GraphQL cache, the only cached data route) is not served to a request
// bearing a different credential, even for the same login.
func TestCredentialIsolation(t *testing.T) {
	router, store := setupTestRouter(t)

	// Seed org repos visible only to testToken's bucket.
	store.SetOrgRepos(seedCtx(), "my-org", []dbgen.Repo{
		{Owner: "my-org", Name: "secret", NameWithOwner: "my-org/secret", Url: "u"},
	})

	// A different token (same stubbed login) resolves to a different bucket; the
	// org-repos query must return nothing for it.
	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer other-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	nodes := resp["data"].(map[string]interface{})["organization"].(map[string]interface{})["repositories"].(map[string]interface{})["nodes"].([]interface{})
	assert.Equal(t, 0, len(nodes), "a different credential must not see another credential's cached repos")
}

// TestWebhook_NoAuthRequired verifies the webhook endpoint is reachable without
// a bearer token (it is authenticated by HMAC signature instead). A ping is an
// untracked event, so it is accepted as a no-op (202), not rejected (401/403).
func TestWebhook_NoAuthRequired(t *testing.T) {
	router, _ := setupTestRouter(t)

	body := "{}"
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, []byte(body)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, webhook.DispIgnored, w.Header().Get("X-GSM-Disposition"))
}
