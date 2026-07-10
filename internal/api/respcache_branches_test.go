package api

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Branches-list route tests (GET /repos/{owner}/{repo}/branches); the shared
// fake upstream (respCacheUpstream) lives in respcache_test.go.

// defaultBranchesUpstream is respCacheUpstream's default /branches answer:
// two branches, one protected -- commit.url, the protection object,
// protection_url, and _links must all be dropped by the rebuild.
func defaultBranchesUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `[
		{"name": "main",
		 "commit": {"sha": %q, "url": "https://api.github.com/repos/org1/repo1/commits/%s"},
		 "protected": true,
		 "protection": {"enabled": true, "required_status_checks": {"enforcement_level": "non_admins", "contexts": ["ci"]}},
		 "protection_url": "https://api.github.com/repos/org1/repo1/branches/main/protection",
		 "_links": {"self": "https://api.github.com/repos/org1/repo1/branches/main"}},
		{"name": "claude/feature-branch",
		 "commit": {"sha": %q, "url": "https://api.github.com/repos/org1/repo1/commits/%s"},
		 "protected": false,
		 "protection": {"enabled": false, "required_status_checks": {"enforcement_level": "off", "contexts": []}},
		 "protection_url": "https://api.github.com/repos/org1/repo1/branches/claude/feature-branch/protection",
		 "_links": {"self": "https://api.github.com/repos/org1/repo1/branches/claude/feature-branch"}}
	]`, shaTip, shaTip, shaMid, shaMid)
}

// TestCachedBranchesList_MissAbsorbHit covers the core flow: the first read
// fetches + absorbs the whole trimmed page (miss), the second serves the
// byte-identical stored doc (hit, zero upstream calls), and the rebuild
// keeps exactly {name, commit.sha, protected} -- commit.url, the protection
// object, protection_url, and _links all dropped.
func TestCachedBranchesList_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/branches"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.branchesHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`[
		{"name": "main", "commit": {"sha": %q}, "protected": true},
		{"name": "claude/feature-branch", "commit": {"sha": %q}, "protected": false}
	]`, shaTip, shaMid), w1.Body.String(), "exact trimmed shape: the protection object must be gone")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.branchesHits), "hit must not call upstream")
}

// TestCachedBranchesList_PushFlush: branch create/delete/tip-move all arrive
// as pushes, so a push (and a repository event) flushes the repo's snapshots.
func TestCachedBranchesList_PushFlush(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/branches"

	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.branchesHits))

	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "a push must flush the branches snapshots")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.branchesHits))

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w3.Header().Get(cacheHeader), "the re-absorbed snapshot serves again")

	postWebhook(t, router, "repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w4 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader), "repository events must flush too")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.branchesHits))
}

// TestCachedBranchesList_DefaultShapeSharesKey: the bare query and GitHub's
// explicit defaults (per_page=30, page=1) resolve to the same snapshot key,
// while a different per_page is its own snapshot.
func TestCachedBranchesList_DefaultShapeSharesKey(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/branches", nil))
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/branches?per_page=30&page=1", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "the bare shape and explicit defaults share a key")
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.branchesHits))

	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/branches?per_page=100", nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "a different per_page is its own snapshot")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.branchesHits))
}

// TestCachedBranchesList_ShapePassthroughs: shapes the cache does not model
// pass through verbatim, uncached, every time.
func TestCachedBranchesList_ShapePassthroughs(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/branches?protected=true", // unmodeled filter
		"/repos/org1/repo1/branches?per_page=101",   // out of range
		"/repos/org1/repo1/branches?page=11",        // beyond the modeled cap
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.branchesHits), target)
	}

	// Passthroughs stored nothing: a cacheable shape still misses.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/branches", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
}

// TestCachedBranchesList_EmptyArrayCacheable: an empty array -- a page past
// the end of the branch list -- is a valid answer for that exact page key.
func TestCachedBranchesList_EmptyArrayCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.branches = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[]`))
	}
	target := "/repos/org1/repo1/branches?per_page=100&page=5"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a page past the end is a valid cacheable answer")
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.branchesHits))
}
