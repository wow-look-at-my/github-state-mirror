package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Owner repo-listing route tests. The default body is a GitHub repos page with
// every URL field present, so the rebuild has something to drop.

const (
	orgReposTarget  = "/orgs/org1/repos"
	userReposTarget = "/users/alice/repos"
)

func ownerRepoObject(owner, name, visibility string) map[string]any {
	return map[string]any{
		"id": 1296269, "node_id": "R_1", "name": name,
		"full_name": owner + "/" + name,
		"owner": map[string]any{
			"login": owner, "id": 1, "avatar_url": "https://a",
			"html_url": "https://github.com/" + owner,
		},
		"private": visibility != "public", "visibility": visibility,
		"default_branch": "main", "archived": false, "disabled": false,
		"fork": false, "description": "a repo",
		"url":       "https://api.github.com/repos/" + owner + "/" + name,
		"html_url":  "https://github.com/" + owner + "/" + name,
		"clone_url": "https://github.com/" + owner + "/" + name + ".git",
		"hooks_url": "https://api.github.com/repos/" + owner + "/" + name + "/hooks",
	}
}

func defaultOwnerReposUpstream(w http.ResponseWriter, r *http.Request) {
	owner := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[1]
	writeGitHubJSON(w, []any{
		ownerRepoObject(owner, "repo1", "public"),
		ownerRepoObject(owner, "repo2", "private"),
	})
}

func TestCachedOwnerRepos_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", orgReposTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `[
		{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"},
		 "private":false,"visibility":"public","default_branch":"main",
		 "archived":false,"disabled":false},
		{"name":"repo2","full_name":"org1/repo2","owner":{"login":"org1"},
		 "private":true,"visibility":"private","default_branch":"main",
		 "archived":false,"disabled":false}
	]`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", orgReposTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.ownerReposHits), "a hit must not call upstream")
}

// The path spellings answer differently for the same name, so scope keys the
// row and an org listing can never answer a user listing.
func TestCachedOwnerRepos_ScopesAreDistinctRows(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", "/orgs/alice/repos", nil)).Header().Get(cacheHeader))
	w := do(t, router, authedReq("GET", userReposTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "the user spelling must not hit the org row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.ownerReposHits))
}

// Every listed repo enters global truth, so a listing warms the bare-repo route
// and the reveal layer's public fast path.
func TestCachedOwnerRepos_AbsorbsIntoTruth(t *testing.T) {
	router, store, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", orgReposTarget, nil)).Header().Get(cacheHeader))

	private, err := store.GetRepoInsensitive(t.Context(), "org1", "repo2")
	require.NoError(t, err)
	assert.Equal(t, "private", private.Visibility)
	assert.Equal(t, "main", private.DefaultBranch.String)

	// A public row is the reveal layer's fast path, so nothing reaches GitHub.
	before := atomic.LoadInt32(&u.probeHits)
	w := do(t, router, authedReq("GET", "/repos/org1/repo1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Equal(t, before, atomic.LoadInt32(&u.probeHits), "truth from the listing must answer without a fetch")
}

// A repository event moves the owner's listing for every credential, since the
// delivery names no token.
func TestCachedOwnerRepos_RepositoryEventFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", orgReposTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", orgReposTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "repository", map[string]any{
		"action": "created", "repository": map[string]any{
			"name": "repo3", "owner": map[string]any{"login": "org1"},
			"full_name": "org1/repo3", "private": false, "visibility": "public",
			"default_branch": "main",
		},
	})

	w := do(t, router, authedReq("GET", orgReposTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a repository event must flush the owner's listing")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.ownerReposHits))
}

// The absent verdict, meaning the name is not an owner of this kind, is stable
// and is stored.
func TestCachedOwnerRepos_AbsentIsStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.ownerRepos = absentReadmeUpstream

	w1 := do(t, router, authedReq("GET", orgReposTarget, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"message":"Not Found","status":"404"}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", orgReposTarget, nil))
	assert.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.ownerReposHits))
}

// Shape guards: anything the route does not model forwards with a counted
// reason instead of vanishing.
func TestCachedOwnerRepos_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// An unmodeled parameter (the type filter is deliberately not modeled).
	w := do(t, router, authedReq("GET", orgReposTarget+"?type=forks", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// An undocumented sort value.
	w = do(t, router, authedReq("GET", orgReposTarget+"?sort=stars", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// Pagination past the modeled depth.
	w = do(t, router, authedReq("GET", orgReposTarget+"?page=999", nil))
	assert.Empty(t, w.Header().Get(cacheHeader))

	// A non-default Accept.
	req := authedReq("GET", orgReposTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w = do(t, router, req)
	assert.Empty(t, w.Header().Get(cacheHeader))

	assert.Equal(t, int32(4), atomic.LoadInt32(&u.ownerReposHits), "all must have reached GitHub")
}

// A partial entry cannot be rendered without inventing state, so the page
// relays verbatim and stores nothing.
func TestCachedOwnerRepos_PartialEntryIsNeverStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.ownerRepos = func(w http.ResponseWriter, _ *http.Request) {
		// No visibility: unknown visibility fails closed rather than reading as public.
		writeGitHubJSON(w, []any{map[string]any{
			"name": "repo1", "full_name": "org1/repo1",
			"owner": map[string]any{"login": "org1"}, "default_branch": "main",
		}})
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", orgReposTarget, nil))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.ownerReposHits))
	}
}
