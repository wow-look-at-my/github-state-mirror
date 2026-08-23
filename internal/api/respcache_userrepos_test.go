package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Personalized repo-listing route tests.

func defaultUserReposUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, []any{
		map[string]any{
			"id": 1, "name": "repo1", "full_name": "org1/repo1", "private": true,
			"owner": map[string]any{"login": "org1"}, "url": "https://api.github.com/repos/org1/repo1",
			"permissions": map[string]any{"admin": false, "push": true, "pull": true},
		},
	})
}

func TestCachedUserRepos_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/user/repos", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.userReposHits))
	// Verbatim: permissions (a trim would likely have dropped) rides through.
	// json.Marshal sorts map keys alphabetically, so admin < pull < push.
	assert.Contains(t, w1.Body.String(), `"permissions":{"admin":false,"pull":true,"push":true}`)

	w2 := do(t, router, authedReq("GET", "/user/repos", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.userReposHits), "a hit must not call upstream")
}

func TestCachedUserRepos_NeverSharedAcrossCredentials(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, hooksReq("GET", "/user/repos", "tok-a")).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, hooksReq("GET", "/user/repos", "tok-b")).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.userReposHits))
}

// sort is modeled and part of the key; everything else forwards.
func TestCachedUserRepos_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", "/user/repos?sort=updated&per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/user/repos?sort=pushed&per_page=50&page=1", nil)).Header().Get(cacheHeader),
		"a different sort must not be answered by another sort's row")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/user/repos?sort=updated&per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.userReposHits))

	for _, target := range []string{
		"/user/repos?affiliation=owner",
		"/user/repos?sort=stars", // not one of GitHub's documented sort values
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "%s must forward", target)
	}
}
