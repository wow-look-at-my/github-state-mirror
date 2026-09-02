package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hook-listing route tests, repo and org. The default body is GitHub's
// documented `hook` schema with every API self-link present, so the rebuild
// has something to drop -- and with config.url present, which it must KEEP.

func defaultHooksUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, []any{map[string]any{
		"type": "Repository", "id": 12345678, "name": "web", "active": true,
		"events": []any{"push", "pull_request"},
		"config": map[string]any{
			"content_type": "json", "insecure_ssl": "0",
			"url":    "https://hooks.example.com/ingest" + r.URL.Path,
			"secret": "********",
		},
		"updated_at": "2026-08-01T00:00:00Z", "created_at": "2026-07-01T00:00:00Z",
		"url":            hookAPIURL(r.URL.Path, ""),
		"test_url":       hookAPIURL(r.URL.Path, "/test"),
		"ping_url":       hookAPIURL(r.URL.Path, "/pings"),
		"deliveries_url": hookAPIURL(r.URL.Path, "/deliveries"),
		"last_response":  map[string]any{"code": 200, "status": "active", "message": "OK"},
	}})
}

// hookAPIURL is of the hook object's API self-links, built here rather
// than inside the JSON so the value is placed by %q and escaped.
func hookAPIURL(path, suffix string) string {
	return "https://api.github.com" + path + "/12345678" + suffix
}

const (
	repoHooksTarget = "/repos/org1/repo1/hooks"
	orgHooksTarget  = "/orgs/org1/hooks"
)

// hooksAllowedURLKeys: the hook config's own url, not a GitHub API link.
var hooksAllowedURLKeys = []string{"url"}

func TestCachedHooks_MissAbsorbHit(t *testing.T) {
	for _, tc := range []struct {
		name, target string
	}{
		{"repo", repoHooksTarget},
		{"org", orgHooksTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, _, u := respCacheStack(t)

			w1 := do(t, router, authedReq("GET", tc.target, nil))
			require.Equal(t, http.StatusOK, w1.Code)
			assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
			assert.Equal(t, int32(1), atomic.LoadInt32(&u.hooksHits))
			assertNoURLKeys(t, w1.Body.Bytes(), hooksAllowedURLKeys...)

			// The API self-links are gone; the hook's own destination stays.
			body := w1.Body.String()
			assert.Contains(t, body, `"url":"https://hooks.example.com/ingest`)
			assert.NotContains(t, body, "api.github.com")
			assert.NotContains(t, body, "test_url")
			assert.NotContains(t, body, "ping_url")
			assert.NotContains(t, body, "deliveries_url")
			// The mask's presence answers "is a secret configured".
			assert.Contains(t, body, `"secret":"********"`)
			assert.Contains(t, body, `"last_response":{"code":200,"status":"active","message":"OK"}`)

			w2 := do(t, router, authedReq("GET", tc.target, nil))
			assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
			assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
			assert.Equal(t, int32(1), atomic.LoadInt32(&u.hooksHits), "a hit must not call upstream")
		})
	}
}

// Rows are keyed by credential: an admin-only answer must never reach a
// read-only principal via the reveal layer's public fast path.
func TestCachedHooks_NeverSharedAcrossCredentials(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	first := do(t, router, hooksReq("GET", repoHooksTarget, "admin-token"))
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, "miss", first.Header().Get(cacheHeader))

	second := do(t, router, hooksReq("GET", repoHooksTarget, "read-only-token"))
	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, "miss", second.Header().Get(cacheHeader),
		"an admin-only answer must never be replayed to a credential GitHub has not answered")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.hooksHits), "the second credential must reach GitHub itself")

	// And each then serves from its own row.
	assert.Equal(t, "hit", do(t, router, hooksReq("GET", repoHooksTarget, "admin-token")).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.hooksHits))
}

// The repo and org listings are separate questions about the same login and
// must never answer each other.
func TestCachedHooks_ScopesAreDistinct(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", repoHooksTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", orgHooksTarget, nil)).Header().Get(cacheHeader),
		"the org listing must not be answered by the repo row for the same login")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.hooksHits))
}

// A write flushes the target's listings across every credential.
func TestCachedHooks_WriteFlushesEveryCredential(t *testing.T) {
	for _, tc := range []struct {
		name, get, write, method string
	}{
		{"repo create", repoHooksTarget, repoHooksTarget, "POST"},
		{"repo edit", repoHooksTarget, repoHooksTarget + "/12345678", "PATCH"},
		{"repo delete", repoHooksTarget, repoHooksTarget + "/12345678", "DELETE"},
		{"org create", orgHooksTarget, orgHooksTarget, "POST"},
		{"org edit", orgHooksTarget, orgHooksTarget + "/12345678", "PATCH"},
		{"org delete", orgHooksTarget, orgHooksTarget + "/12345678", "DELETE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, _, _ := respCacheStack(t)

			//  different credentials each warm their own row.
			require.Equal(t, "miss", do(t, router, hooksReq("GET", tc.get, "tok-a")).Header().Get(cacheHeader))
			require.Equal(t, "miss", do(t, router, hooksReq("GET", tc.get, "tok-b")).Header().Get(cacheHeader))
			require.Equal(t, "hit", do(t, router, hooksReq("GET", tc.get, "tok-a")).Header().Get(cacheHeader))

			//  of them writes.
			wr := do(t, router, hooksReq(tc.method, tc.write, "tok-a"))
			require.Less(t, wr.Code, 400)
			assert.Empty(t, wr.Header().Get(cacheHeader), "a write is forwarded, never rebuilt")

			// BOTH credentials must now miss, not just the writer.
			assert.Equal(t, "miss", do(t, router, hooksReq("GET", tc.get, "tok-a")).Header().Get(cacheHeader),
				"the writer must not read back its own stale listing")
			assert.Equal(t, "miss", do(t, router, hooksReq("GET", tc.get, "tok-b")).Header().Get(cacheHeader),
				"another caller must not keep reconciling against a listing the write invalidated")
		})
	}
}

// repository events are the ONLY delivery that flushes these rows.
func TestCachedHooks_RepositoryEventFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", repoHooksTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", repoHooksTarget, nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "repository", `{"action":"renamed","repository":{"name":"repo1","owner":{"login":"org1"},"private":true,"visibility":"private","default_branch":"main","full_name":"org1/repo1"}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", repoHooksTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.hooksHits))
}

// Shape guards: pages are separate rows, and anything unmodeled forwards with
// a counted reason rather than minting.
func TestCachedHooks_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", repoHooksTarget+"?per_page=100&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", repoHooksTarget+"?per_page=100&page=2", nil)).Header().Get(cacheHeader),
		"page 2 must not be answered by page 1's row")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", repoHooksTarget+"?per_page=100&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.hooksHits))

	for _, target := range []string{
		repoHooksTarget + "?per_page=0",
		repoHooksTarget + "?per_page=101",
		fmt.Sprintf("%s?page=%d", repoHooksTarget, hooksMaxCachedPage+1),
		repoHooksTarget + "?sort=created",
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "%s must forward", target)
	}

	// A non-default Accept forwards too.
	req := authedReq("GET", repoHooksTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(7), atomic.LoadInt32(&u.hooksHits))
}

// A relays unstored: a permission grant changes with no event to catch it.
func TestCachedHooks_ForbiddenRelayedUnstored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.hooks = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Must have admin rights to Repository."}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", repoHooksTarget, nil))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.hooksHits), "a refusal must never be served from state")
	}
}

// An empty listing is a real, cacheable answer: "this repo has no hooks" is
// precisely what a reconciliation sweep is asking.
func TestCachedHooks_EmptyListingIsCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.hooks = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[]`))
	}

	w1 := do(t, router, authedReq("GET", repoHooksTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "[]", w1.Body.String())
	assert.Equal(t, "hit", do(t, router, authedReq("GET", repoHooksTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.hooksHits))
}

// insecure_ssl is documented as a string OR a number, so it rides as raw JSON:
// coercing it would change the shape for whichever form the caller sent.
func TestCachedHooks_InsecureSSLKeepsItsJSONType(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.hooks = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[{"id":1,"name":"web","active":true,"events":["push"],
			"config":{"url":"https://h/x","content_type":"json","insecure_ssl":1},
			"created_at":"","updated_at":""}]`))
	}

	w := do(t, router, authedReq("GET", repoHooksTarget, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"insecure_ssl":1`)
	assert.NotContains(t, w.Body.String(), `"insecure_ssl":"1"`)
	// A hook with no secret configured must not grow.
	assert.NotContains(t, w.Body.String(), "secret")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.hooksHits))
}

// hooksReq builds a request to a hook path with a DISTINCT bearer, so the
// per-credential key can be exercised across the read and write verbs.
func hooksReq(method, target, bearer string) *http.Request {
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPatch {
		body = strings.NewReader(`{"config":{"url":"https://hooks.example.com/ingest"}}`)
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req
}
