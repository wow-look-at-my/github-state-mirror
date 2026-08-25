package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR commits-list tests (GET /repos/{owner}/{repo}/pulls/{number}/commits): this route reuses the repo commits-list storage.

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

// A pull_request delivery flushes only THAT PR's snapshots, leaving other PRs alone.
func TestCachedPullCommits_PullRequestEventFlushesThatPR(t *testing.T) {
	router, _, _, _ := commitsCacheStack(t)

	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil))
	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/commits", nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/commits", nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/commits", nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "pull_request", map[string]any{
		"action": "synchronize", "number": 7,
		"pull_request": map[string]any{
			"number": 7, "state": "open", "title": "t",
			"head": map[string]any{"ref": "feature", "sha": shaTip},
			"base": map[string]any{"ref": "main", "sha": shaBase},
		},
		"repository": fixtureRepo(),
	})

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

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/feature", "after": shaTip, "repository": fixtureRepo(),
	})

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

// The dispatcher restates this key literal (internal/sync cannot import internal/api).
func TestPullCommitsRefKeyLiteral(t *testing.T) {
	assert.Equal(t, "pull/7/commits", pullCommitsRefKey(7))
}
