package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Git-tree route tests. The default body carries the full documented shape
// (per-entry url, plus size on the blob entry only) so the rebuild has
// something to drop.

func defaultGitTreeUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, map[string]any{
		"sha": shaTree1,
		"url": "https://api.github.com" + r.URL.Path,
		"tree": []any{
			map[string]any{
				"path": "README.md", "mode": "100644", "type": "blob",
				"sha": shaBase, "size": 42, "url": "https://api.github.com/blobs/x",
			},
			map[string]any{
				"path": "src", "mode": "040000", "type": "tree",
				"sha": shaMid, "url": "https://api.github.com/trees/y",
			},
		},
		"truncated": false,
	})
}

const gitTreeTarget = "/repos/org1/repo1/git/trees/" + shaTree1

func TestCachedGitTree_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", gitTreeTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitTreeHits))
	assertNoURLKeys(t, w1.Body.Bytes())

	body := w1.Body.String()
	assert.Contains(t, body, `"sha":"`+shaTree1+`"`)
	assert.Contains(t, body, `"path":"README.md"`)
	assert.Contains(t, body, `"size":42`)
	assert.Contains(t, body, `"path":"src"`)
	assert.NotContains(t, body, "api.github.com")

	w2 := do(t, router, authedReq("GET", gitTreeTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitTreeHits), "a hit must not call upstream")
}

// recursive= is a DIFFERENT resource (a different entry set for the same
// sha), so it must never be answered by the non-recursive row or vice versa.
func TestCachedGitTree_RecursiveIsADistinctRow(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", gitTreeTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", gitTreeTarget+"?recursive=1", nil)).Header().Get(cacheHeader),
		"recursive=1 must not be answered by the non-recursive row")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", gitTreeTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", gitTreeTarget+"?recursive=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.gitTreeHits))
}

// Trees are immutable: a repository event that would flush every other
// ref-relative cache must NOT touch a stored tree.
func TestCachedGitTree_ImmutableAcrossWebhooks(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", gitTreeTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(), "commits": []any{},
	})
	postWebhook(t, router, "repository", `{"action":"renamed","repository":{"name":"repo1","owner":{"login":"org1"},"private":true,"visibility":"private","default_branch":"main","full_name":"org1/repo1"}}`)

	assert.Equal(t, "hit", do(t, router, authedReq("GET", gitTreeTarget, nil)).Header().Get(cacheHeader),
		"a tree is content-addressed and immutable; no webhook should ever invalidate it")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitTreeHits))
}

// Shape guards: a short sha, a non-default Accept, and an unmodeled query
// parameter all forward with a counted reason instead of minting a row.
func TestCachedGitTree_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	for _, target := range []string{
		"/repos/org1/repo1/git/trees/" + shaTree1[:7], // short sha
		gitTreeTarget + "?recursive=0",                // GitHub only defines recursive=
		gitTreeTarget + "?per_page=1",
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "%s must forward", target)
	}

	req := authedReq("GET", gitTreeTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.gitTreeHits), "every unmodeled shape forwards to GitHub uncached")
}

// A (bad or GC'd sha) relays unstored -- no miss-marker table exists for
// this route (see the file header).
func TestCachedGitTree_NotFoundRelayedUnstored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.gitTree = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", gitTreeTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.gitTreeHits), "a 404 must never be served from state on this route")
	}
}
