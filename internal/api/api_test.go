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
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// testToken is the bearer token sent by authenticated test requests.
const testToken = "test-token"

// testUserID/testUserLogin are the identity the default fake GitHub answers on
// GET /user for any token; testUserActor is therefore the principal every
// authenticated test request resolves to ("user:<id>").
const (
	testUserID    = 7001
	testUserLogin = "testuser"
	testUserActor = "user:7001"
)

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
	// Stub GitHub's /user endpoint so requireAuth can resolve the test token to
	// a user (id + login) without reaching the real API.
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
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
	s := newFullTestStack(t, authSvc, ghHandler)
	return s.router, s.store, s.db, s.ghURL
}

// testStack is the full router assembly for tests that also need the
// subscriber-notification pieces (the notifier for deterministic flushes) or
// the timeline ring (for asserting timed traffic events).
type testStack struct {
	router   http.Handler
	store    *ghdata.Store
	db       *sql.DB
	ghURL    string
	notifier *notify.Notifier
	timeline *reqtimeline.Recorder
}

func newFullTestStack(t *testing.T, authSvc *auth.Service, ghHandler http.Handler) testStack {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Open(dbPath)
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	// Register a stub fetcher so EnsureFresh doesn't panic.
	mgr.RegisterFetcher(freshness.Policy{Kind: syncpkg.KindOrgRepos}, &stubFetcher{})

	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)

	// Subscriptions live in their own DB file, like production. Single-attempt
	// deliveries keep tests deterministic (retry behavior is covered by the
	// notify package's own tests); Drain before the DB closes, like main.
	subs, err := notify.Open(filepath.Join(dir, "subscriptions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { subs.Close() })
	notifier := notify.New(notify.Config{
		Store: subs, Access: store,
		Attempts: 1, AttemptTimeout: 5 * time.Second, Backoff: []time.Duration{time.Millisecond},
	})
	t.Cleanup(func() { notifier.Drain(5 * time.Second) })

	ghSrv := httptest.NewServer(ghHandler)
	t.Cleanup(ghSrv.Close)
	gh := ghclient.NewWithBaseURL(ghSrv.URL)

	// Passive rate-limit meter, wired like cmd/server: the ghclient hook plus
	// the router's proxy/fetch/probe paths all feed it.
	meter := ratemeter.New()
	gh.SetRateObserver(meter.Observe)

	// nil app -> the consistency checker reports Available()==false, the realistic
	// "no GitHub App configured" state for these tests.
	checker := syncpkg.NewConsistencyChecker(gh, store, fStore, nil)
	timeline := reqtimeline.New()
	// Wire the client's exchange observer like cmd/server does, so tests see
	// the ghclient calls (e.g. requireAuth's /user resolution) on the chart.
	gh.SetExchangeObserver(TimelineExchangeObserver(timeline))
	router := NewRouter(mgr, store, testWebhookSecret, dispatcher, gh, []string{"*"}, authSvc, "", checker, meter, notifier, dbPath, timeline)
	return testStack{router: router, store: store, db: db, ghURL: ghSrv.URL, notifier: notifier, timeline: timeline}
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

// seedCtx returns a context carrying the test user's principal, for store
// operations that read the actor from context.
func seedCtx() context.Context {
	return actor.WithActor(context.Background(), testUserActor)
}

// seedOrgTruth merges repos (and optional PRs) into global truth AS IF the
// given principal had run an org list-sync: truth rows are written and the
// principal earns list_sync grants for every repo.
func seedOrgTruth(t *testing.T, store *ghdata.Store, principal, owner string, repos []dbgen.Repo, prsByRepo map[string][]dbgen.PullRequest) {
	t.Helper()
	data := ghdata.OrgSyncData{
		Repos:      repos,
		PRsByRepo:  prsByRepo,
		LabelsByPR: map[string]map[int64][]dbgen.PrLabel{},
	}
	now := time.Now()
	require.NoError(t, store.SyncOrgTruth(context.Background(), owner, data, principal, now, now))
}

// authedReq builds a request carrying a valid bearer token.
func authedReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

// TestRequireAuth_Unauthenticated verifies that data endpoints reject requests
// with no Authorization header. Non-cached REST paths fall through to the
// passthrough proxy, which itself enforces the token; the GraphQL route is
// gated by requireAuth. Either way a tokenless request is 401.
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

// TestRevealIsolation verifies the reveal layer: global truth absorbed via one
// principal's sync (a PRIVATE-by-default repo, since the GraphQL sync cannot
// learn visibility) is NOT revealed to a different user with no grant, while
// the syncing user sees it via their list_sync grant.
func TestRevealIsolation(t *testing.T) {
	// Fake GitHub resolves each token to its own user.
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/user", r.URL.Path)

		switch r.Header.Get("Authorization") {
		case "Bearer " + testToken:
			_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
		case "Bearer other-user-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "otheruser", "id": 8002})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	router, store, _, _ := newTestStackWithGitHub(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), gh)

	// Global truth learns about the repo from the test user's sync; only that
	// user is granted (visibility unknown -> treated private, fail closed).
	seedOrgTruth(t, store, testUserActor, "my-org", []dbgen.Repo{
		{Owner: "my-org", Name: "secret", NameWithOwner: "my-org/secret", Url: "u"},
	}, nil)

	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	nodesFor := func(token string) []interface{} {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		return resp["data"].(map[string]interface{})["organization"].(map[string]interface{})["repositories"].(map[string]interface{})["nodes"].([]interface{})
	}

	assert.Len(t, nodesFor(testToken), 1, "the syncing user's grant reveals the repo")
	assert.Len(t, nodesFor("other-user-token"), 0, "an ungranted user must not see a non-public repo")
}

// TestRevealPublicFastPath verifies that a repo whose visibility is PUBLIC in
// global truth is revealed to any authenticated principal, grant or not.
func TestRevealPublicFastPath(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + testToken:
			_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
		case "Bearer other-user-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "otheruser", "id": 8002})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	router, store, _, _ := newTestStackWithGitHub(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), gh)

	// Truth knows the repo is public (e.g. a webhook's repository object said
	// so). No grants exist for anyone.
	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "my-org", Name: "open", NameWithOwner: "my-org/open", Url: "u",
		Visibility: ghdata.VisibilityPublic,
	}))
	// The other user's org marker must be fresh or the stub fetcher runs (a
	// no-op) — either way assembly filters; both users should see the repo.
	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	for _, token := range []string{testToken, "other-user-token"} {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		nodes := resp["data"].(map[string]interface{})["organization"].(map[string]interface{})["repositories"].(map[string]interface{})["nodes"].([]interface{})
		assert.Len(t, nodes, 1, "a public repo is revealed to any principal (token %s)", token)
	}
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
