package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ref-prefix-search route tests.

func defaultMatchingRefsUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, []any{
		map[string]any{
			"ref": "refs/heads/pr-minder-queue/1", "node_id": "MDM6UmVmMQ==",
			"url":    "https://api.github.com" + r.URL.Path + "/1",
			"object": map[string]any{"sha": shaBase, "type": "commit", "url": "https://api.github.com/commits/x"},
		},
		map[string]any{
			"ref": "refs/heads/pr-minder-queue/2", "node_id": "MDM6UmVmMg==",
			"url":    "https://api.github.com" + r.URL.Path + "/2",
			"object": map[string]any{"sha": shaMid, "type": "commit", "url": "https://api.github.com/commits/y"},
		},
	})
}

const matchingRefsTarget = "/repos/org1/repo1/git/matching-refs/heads/pr-minder-queue/"

func TestCachedMatchingRefs_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", matchingRefsTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.matchingRefsHits))
	assertNoURLKeys(t, w1.Body.Bytes())

	body := w1.Body.String()
	assert.Contains(t, body, `"ref":"refs/heads/pr-minder-queue/1"`)
	assert.Contains(t, body, `"sha":"`+shaBase+`"`)
	assert.NotContains(t, body, "api.github.com")
	assert.NotContains(t, body, "node_id")
	assert.NotContains(t, body, `"type"`)

	w2 := do(t, router, authedReq("GET", matchingRefsTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.matchingRefsHits), "a hit must not call upstream")
}

// An empty match set is a real, cacheable answer.
func TestCachedMatchingRefs_EmptyIsCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.matchingRefs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[]`))
	}

	w1 := do(t, router, authedReq("GET", matchingRefsTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "[]", w1.Body.String())
	assert.Equal(t, "hit", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.matchingRefsHits))
}

// A push under refs/heads/ flushes the repo's pages -- no narrower per-prefix
// target exists (see the file header in respcache_matchingrefs.go).
func TestCachedMatchingRefs_PushFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/pr-minder-queue/3", "before": "0000000000000000000000000000000000000000", "after": shaTip,
		"repository": fixtureRepo(), "commits": []any{}, "created": true,
	})

	assert.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader),
		"a new queue branch changes the prefix search's answer")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.matchingRefsHits))
}

// A repository event flushes too (rename/delete moves the whole answer).
func TestCachedMatchingRefs_RepositoryEventFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "repository", `{"action":"renamed","repository":{"name":"repo1","owner":{"login":"org1"},"private":true,"visibility":"private","default_branch":"main","full_name":"org1/repo1"}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.matchingRefsHits))
}

// Different prefixes are different rows.
func TestCachedMatchingRefs_PrefixesAreDistinct(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/repos/org1/repo1/git/matching-refs/heads/other/", nil)).Header().Get(cacheHeader),
		"a different prefix must not be answered by another prefix's row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.matchingRefsHits))
}

// Shape guards: pages are separate rows, and a non-default Accept forwards.
func TestCachedMatchingRefs_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget+"?per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", matchingRefsTarget+"?per_page=50&page=2", nil)).Header().Get(cacheHeader),
		"page 2 must not be answered by page 1's row")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", matchingRefsTarget+"?per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.matchingRefsHits))

	req := authedReq("GET", matchingRefsTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.matchingRefsHits))
}
