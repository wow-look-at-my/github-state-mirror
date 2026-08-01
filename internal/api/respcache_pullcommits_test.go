package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR commits-list tests (GET /repos/{owner}/{repo}/pulls/{number}/commits).
// The fake upstream is the commits-list one (respcache_commits_test.go): it
// answers any path ending in /commits, which is the point — GitHub gives both
// routes the same item shape, and this route reuses that storage.

func TestCachedPullCommits_MissAbsorbHit(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7/commits"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))
	assertNoURLKeys(t, w1.Body.Bytes())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must serve the same shape")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits), "a hit must not call upstream")
}

// The synthetic ref key must not collide with the repository commits list:
// two different resources, two different rows.
func TestCachedPullCommits_DoesNotCollideWithRepoCommits(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)

	do(t, router, authedReq("GET", "/repos/org1/repo1/commits", nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader),
		"the PR's commits must not be answered by the repository listing's row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))

	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/commits", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// A pull_request delivery flushes THAT PR's snapshots — the per-PR signal
// that also covers fork heads, whose pushes never reach us — and leaves other
// PRs alone.
func TestCachedPullCommits_PullRequestEventFlushesThatPR(t *testing.T) {
	router, _, _, _ := commitsCacheStack(t)

	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil))
	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/commits", nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/commits", nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "pull_request", `{"action":"synchronize","number":7,
		"pull_request":{"number":7,"state":"open","title":"t","head":{"ref":"feature","sha":"`+shaTip+`"},
		"base":{"ref":"main","sha":"`+shaBase+`"}},
		"repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil)).Header().Get(cacheHeader),
		"the synchronized PR's commits must be flushed")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/commits", nil)).Header().Get(cacheHeader),
		"another PR's commits must survive")
}

// A push is the repo-wide belt for a missed pull_request delivery, and it must
// not leave a PR's snapshot behind.
func TestCachedPullCommits_PushFlushesPRSnapshots(t *testing.T) {
	router, _, _, _ := commitsCacheStack(t)

	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "push", `{"ref":"refs/heads/feature","after":"`+shaTip+`",
		"repository":{"name":"repo1","owner":{"login":"org1"},"default_branch":"main"}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil)).Header().Get(cacheHeader))
}

func TestCachedPullCommits_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, _ := commitsCacheStack(t)

	for _, tc := range []struct{ name, target, accept string }{
		{"non-numeric number", "/repos/org1/repo1/pulls/comments/commits", ""},
		{"deep page", "/repos/org1/repo1/pulls/7/commits?page=99", ""},
		{"unknown parameter", "/repos/org1/repo1/pulls/7/commits?since=2026-01-01", ""},
		{"non-default accept", "/repos/org1/repo1/pulls/7/commits", "application/vnd.github.diff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedReq("GET", tc.target, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			w := do(t, router, req)
			assert.Empty(t, w.Header().Get(cacheHeader), "must be forwarded, not served from the cache")
		})
	}
}

// The dispatcher restates this key (internal/sync cannot import internal/api).
// Both sides pin the same literal; the pull_request flush test above proves
// they still meet end to end.
func TestPullCommitsRefKeyLiteral(t *testing.T) {
	assert.Equal(t, "pull/7/commits", pullCommitsRefKey(7))
}
