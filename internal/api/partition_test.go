package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// Per-user principal resolution (1 GitHub user == 1 reveal-layer principal).
//
// These tests cover requireAuth's principal selection end to end against a
// fake GitHub upstream:
//   - two different tokens of the same user share one "user:<id>" principal
//     (grants earned by either token reveal truth to both)
//   - a machine token (403 on /user) falls back to a per-token fingerprint
//     principal, with the verdict cached
//   - a transient /user failure fails the request (503) and never picks a
//     principal; recovery needs no restart
//   - identity rows are recorded under the "user:<id>" principal

func partitionAuthSvc() *auth.Service {
	return auth.New(auth.Config{SessionKey: []byte("test-session-key")})
}

func orgRepoNodes(t *testing.T, body []byte) []interface{} {
	t.Helper()
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp["data"].(map[string]interface{})["organization"].(map[string]interface{})["repositories"].(map[string]interface{})["nodes"].([]interface{})
}

func postOrgRepos(router http.Handler, token string) *httptest.ResponseRecorder {
	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestUserPartition_SameUserTokensShareScope verifies the operator-decided
// keying: two DIFFERENT tokens resolving to the SAME GitHub user id share one
// "user:<id>" principal — grants earned under it reveal truth through either
// token — while a third user's token has no grant and sees nothing. It also
// verifies /user is asked once per unique token (the identity is cached).
func TestUserPartition_SameUserTokensShareScope(t *testing.T) {
	var userCalls atomic.Int64
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/user", r.URL.Path)

		userCalls.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer pat-laptop", "Bearer pat-ci":
			// Two distinct credentials, one human.
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "id": 42})
		case "Bearer someone-elses-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "stranger", "id": 43})
		default:
			t.Errorf("unexpected token: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	router, store, _, _ := newTestStackWithGitHub(t, partitionAuthSvc(), gh)

	// Absorb a repo into global truth via octocat's sync (as a request through
	// any of octocat's tokens would), granting the "user:42" principal.
	seedOrgTruth(t, store, "user:42", "my-org", []dbgen.Repo{
		{Owner: "my-org", Name: "shared-repo", NameWithOwner: "my-org/shared-repo", Url: "u"},
	}, nil)

	// Granted via one token's principal, revealed via the other token.
	for _, token := range []string{"pat-laptop", "pat-ci"} {
		w := postOrgRepos(router, token)
		require.Equal(t, http.StatusOK, w.Code, "token %s", token)
		nodes := orgRepoNodes(t, w.Body.Bytes())
		require.Len(t, nodes, 1, "token %s must read the shared user partition", token)
		assert.Equal(t, "shared-repo", nodes[0].(map[string]interface{})["name"])
	}

	// A different user's token holds no grant and must see nothing.
	w := postOrgRepos(router, "someone-elses-token")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, orgRepoNodes(t, w.Body.Bytes()), 0, "distinct users stay isolated")

	// Identity resolution is cached per token: repeat requests add no /user calls.
	before := userCalls.Load()
	_ = postOrgRepos(router, "pat-laptop")
	_ = postOrgRepos(router, "pat-ci")
	assert.Equal(t, before, userCalls.Load(), "cached identities must not re-hit /user")
	assert.Equal(t, int64(3), before, "one /user call per unique token")
}

// TestMachineToken_FingerprintIsolation verifies that a token GitHub
// definitively says is not a user (403 on /user without rate-limit headers —
// e.g. an installation token) keeps a per-token fingerprint principal, that
// the verdict is cached, and that no identity row is written (there is no
// login to attribute).
func TestMachineToken_FingerprintIsolation(t *testing.T) {
	var userCalls atomic.Int64
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/user", r.URL.Path)

		userCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})
	router, store, _, _ := newTestStackWithGitHub(t, partitionAuthSvc(), gh)

	const machineToken = "ghs_machine-token"
	seedOrgTruth(t, store, ghclient.Fingerprint(machineToken), "my-org", []dbgen.Repo{
		{Owner: "my-org", Name: "bot-repo", NameWithOwner: "my-org/bot-repo", Url: "u"},
	}, nil)

	for i := 0; i < 2; i++ {
		w := postOrgRepos(router, machineToken)
		require.Equal(t, http.StatusOK, w.Code)
		nodes := orgRepoNodes(t, w.Body.Bytes())
		require.Len(t, nodes, 1, "machine token must be served from its fingerprint partition")
		assert.Equal(t, "bot-repo", nodes[0].(map[string]interface{})["name"])
	}
	assert.Equal(t, int64(1), userCalls.Load(), "the not-a-user verdict must be cached per token")

	ids, err := store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids, "a non-user token has no login; no identity row must be written")
}

// TestTransientUserFailure_503AndNoPartition verifies the fail-closed rule:
// when GET /user fails transiently (5xx here) and no verdict is cached, the
// request fails with 503 — it is never silently served from a guessed
// (fingerprint) partition — and nothing is recorded. Once GitHub recovers, the
// same token works without a restart (no negative verdict was cached).
func TestTransientUserFailure_503AndNoPartition(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/user", r.URL.Path)

		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "id": 42})
	})
	router, store, _, _ := newTestStackWithGitHub(t, partitionAuthSvc(), gh)

	w := postOrgRepos(router, "flaky-token")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "transient identity failure must be 503, not a guessed principal")
	assert.Contains(t, w.Body.String(), "retry")

	ids, err := store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids, "a failed resolution must not write an identity row")

	// GitHub recovers: the same token resolves now (no stale verdict cached).
	failing.Store(false)
	w = postOrgRepos(router, "flaky-token")
	require.Equal(t, http.StatusOK, w.Code, "recovery must not require a restart")
}

// TestRateLimited403_IsTransientNotMachineVerdict verifies a rate-limited 403
// on /user (X-RateLimit-Remaining: 0) is treated as transient (503), NOT as a
// definitive "not a user" verdict — otherwise a rate-limited USER token would
// be permanently mis-partitioned onto its fingerprint.
func TestRateLimited403_IsTransientNotMachineVerdict(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, partitionAuthSvc(), gh)

	w := postOrgRepos(router, "rate-limited-user-token")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestIdentityRecordedWithUserActor verifies the dashboard-facing identity row
// is written under the new "user:<id>" actor with the resolved login, so scope
// grouping and is_self labeling keep working.
func TestIdentityRecordedWithUserActor(t *testing.T) {
	router, store := setupTestRouter(t) // default fake: testUserID/testUserLogin

	w := postOrgRepos(router, testToken)
	require.Equal(t, http.StatusOK, w.Code)

	ids, err := store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Equal(t, testUserActor, ids[0].Actor)
	assert.Equal(t, testUserLogin, ids[0].Login)
}
