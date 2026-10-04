package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// README route tests. The default body is GitHub's content object with every
// URL field present, so the rebuild has something to drop.

func defaultReadmeUpstream(w http.ResponseWriter, _ *http.Request) {
	writeGitHubJSON(w, map[string]any{
		"type": "file", "encoding": "base64", "size": 42,
		"name": "README.md", "path": "README.md",
		"content": "aGVsbG8=\n", "sha": shaTree1,
		"url": "https://api.github.com/x", "git_url": "https://api.github.com/y",
		"html_url": "https://github.com/z", "download_url": "https://raw.github.com/w",
		"_links": map[string]any{"self": "https://api.github.com/x"},
	})
}

const readmeTarget = "/repos/org1/repo1/readme"

func absentReadmeUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest","status":"404"}`))
}

func TestCachedReadme_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", readmeTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.readmeHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString(map[string]any{
		"type": "file", "encoding": "base64", "size": 42,
		"name": "README.md", "path": "README.md",
		"content": "aGVsbG8=\n", "sha": shaTree1,
	}), w1.Body.String())

	w2 := do(t, router, authedReq("GET", readmeTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.readmeHits), "a hit must not call upstream")
}

// The absent verdict is an answer about the repo, and a fleet polling every
// repo's README spends a large minority of its calls on repos that have none.
func TestCachedReadme_AbsentIsStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.readme = absentReadmeUpstream

	w1 := do(t, router, authedReq("GET", readmeTarget, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{"message":"Not Found","status":"404"}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", readmeTarget, nil))
	assert.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.readmeHits), "the absent verdict must not be re-fetched")
}

// The path names a DIRECTORY, so the response's own path is what the rebuild
// reports; the row is keyed by the subtree the caller asked under.
func TestCachedReadme_SubtreeIsItsOwnRow(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.readme = func(w http.ResponseWriter, r *http.Request) {
		path := "README.md"
		if r.URL.Path == readmeTarget+"/docs" {
			path = "docs/README.md"
		}
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "base64", "size": 1,
			"name": "README.md", "path": path, "content": "eA==", "sha": shaTree1,
			"url": "https://api.github.com/x",
		})
	}

	root := do(t, router, authedReq("GET", readmeTarget, nil))
	require.Equal(t, "miss", root.Header().Get(cacheHeader))
	assert.Contains(t, root.Body.String(), `"path":"README.md"`)

	sub := do(t, router, authedReq("GET", readmeTarget+"/docs", nil))
	require.Equal(t, "miss", sub.Header().Get(cacheHeader), "a subtree must not hit the root's row")
	assert.Contains(t, sub.Body.String(), `"path":"docs/README.md"`)
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.readmeHits))
}

// Rows key the VERBATIM requested ref, as contents rows do.
func TestCachedReadme_RefsAreDistinctRows(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", readmeTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", readmeTarget, nil)).Header().Get(cacheHeader))

	w := do(t, router, authedReq("GET", readmeTarget+"?ref=dev", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "another ref must not hit the default row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.readmeHits))
}

// A push to the default branch flushes the empty-ref rows, since '' means "the
// default branch" to this route exactly as it does to contents.
func TestCachedReadme_PushToDefaultBranchFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", readmeTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", readmeTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(), "commits": []any{},
	})

	w := do(t, router, authedReq("GET", readmeTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a default-branch push must flush the empty-ref row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.readmeHits))
}

// A push to another branch leaves the default branch's answer alone.
func TestCachedReadme_PushToOtherBranchKeepsTheDefaultRow(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", readmeTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/feature", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(), "commits": []any{},
	})

	w := do(t, router, authedReq("GET", readmeTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "a push elsewhere must not flush the default branch's row")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.readmeHits))
}

// Shape guards: anything the route does not model forwards with a counted
// reason instead of vanishing.
func TestCachedReadme_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// An unmodeled query parameter (the endpoint takes ref and nothing else).
	w := do(t, router, authedReq("GET", readmeTarget+"?per_page=1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// A non-default Accept: the raw representation is a different document.
	req := authedReq("GET", readmeTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w = do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	assert.Equal(t, int32(2), atomic.LoadInt32(&u.readmeHits), "both must have reached GitHub")
}

// An unmodeled shape relays verbatim and stores nothing, so the next read still
// reaches GitHub rather than replaying a hole.
func TestCachedReadme_UnmodeledShapeIsNeverStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.readme = func(w http.ResponseWriter, _ *http.Request) {
		// The oversized form: GitHub reports the size and withholds the content.
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "none", "size": 9000000,
			"name": "README.md", "path": "README.md", "content": "", "sha": shaTree1,
		})
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", readmeTarget, nil))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.readmeHits))
	}
}
