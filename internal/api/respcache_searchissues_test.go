package api

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Search-results route tests. The security property under test is the
// this route exists to protect: search results are permission-scoped per
// GitHub token, so credentials asking the identical query must never
// share a row.

func defaultSearchIssuesUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, map[string]any{
		"total_count":        1,
		"incomplete_results": false,
		"items": []any{
			map[string]any{
				"id": 1, "number": 42, "title": "a bug",
				"url":            "https://api.github.com/repos/org1/repo1/issues/42",
				"html_url":       "https://github.com/org1/repo1/issues/42",
				"repository_url": "https://api.github.com/repos/org1/repo1",
			},
		},
	})
}

func TestCachedSearchIssues_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/search/issues?q=repo:org1/repo1+is:open", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.searchIssuesHits))
	// Verbatim: repository_url (a trim would likely have dropped) rides through.
	assert.Contains(t, w1.Body.String(), `"repository_url"`)

	w2 := do(t, router, authedReq("GET", "/search/issues?q=repo:org1/repo1+is:open", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.searchIssuesHits), "a hit must not call upstream")
}

// The security property: credentials asking the identical query must
// never share a cached row, because GitHub's own results differ by caller.
func TestCachedSearchIssues_NeverSharedAcrossCredentials(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, hooksReq("GET", "/search/issues?q=is:open", "tok-a")).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, hooksReq("GET", "/search/issues?q=is:open", "tok-b")).Header().Get(cacheHeader),
		"a different credential's identical query must not be answered by another credential's row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.searchIssuesHits))
}

func TestCachedSearchIssues_DifferentQueryDifferentRow(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", "/search/issues?q=is:open", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/search/issues?q=is:closed", nil)).Header().Get(cacheHeader),
		"a different query must not be answered by another query's row")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/search/issues?q=is:open", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.searchIssuesHits))
}

// The TTL is the ONLY bound on this route (no webhook shortens it), so a row
// past its expiry must be treated as a miss, not served stale.
func TestCachedSearchIssues_ExpiredRowIsAMiss(t *testing.T) {
	router, store, _, u := respCacheStack(t)

	fp := ghclient.Fingerprint(testToken)
	key := ghdata.SearchIssuesQueryKey("is:open", 30, 1)
	longAgo := time.Now().Add(-time.Hour)
	require.NoError(t, store.PutCachedSearchIssues(t.Context(), fp, key, `{"total_count":0,"items":[]}`, longAgo, ghdata.SearchIssuesCacheTTL))

	w := do(t, router, authedReq("GET", "/search/issues?q=is:open", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired row must not be served")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.searchIssuesHits))
}

// q is required and modeled; per_page/page are modeled; everything else
// (sort, order) forwards.
func TestCachedSearchIssues_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", "/search/issues?q=is:open&per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/search/issues?q=is:open&per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.searchIssuesHits))

	for _, target := range []string{
		"/search/issues", // missing q
		"/search/issues?q=is:open&sort=created",
		"/search/issues?q=is:open&order=desc",
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "%s must forward", target)
	}
}
