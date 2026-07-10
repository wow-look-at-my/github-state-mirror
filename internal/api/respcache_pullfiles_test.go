package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR-files route tests (GET /repos/{owner}/{repo}/pulls/{number}/files); the
// shared fake upstream (respCacheUpstream) lives in respcache_test.go, and
// the PR webhook fixtures (upstreamPR, prEvent) in respcache_pulls_test.go.

// defaultPullFilesUpstream is respCacheUpstream's default /pulls/{n}/files
// answer: two files -- one modified with a patch, one renamed WITHOUT a patch
// (the binary/oversized case) but with previous_filename -- so the rebuild
// must preserve exactly that presence split while dropping sha and every URL.
func defaultPullFilesUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`[
		{"sha": "bbcd538c8e72b8c175046e27cc8f907076331401",
		 "filename": "internal/api/router.go", "status": "modified",
		 "additions": 25, "deletions": 3, "changes": 28,
		 "blob_url": "https://github.com/org1/repo1/blob/x/internal/api/router.go",
		 "raw_url": "https://github.com/org1/repo1/raw/x/internal/api/router.go",
		 "contents_url": "https://api.github.com/repos/org1/repo1/contents/internal/api/router.go?ref=x",
		 "_links": {"self": "https://api.github.com/repos/org1/repo1/pulls/7/files"},
		 "patch": "@@ -1,3 +1,25 @@ package api"},
		{"sha": "cafe538c8e72b8c175046e27cc8f907076331402",
		 "filename": "docs/new-name.md", "status": "renamed",
		 "additions": 0, "deletions": 0, "changes": 0,
		 "previous_filename": "docs/old-name.md",
		 "blob_url": "https://github.com/org1/repo1/blob/x/docs/new-name.md",
		 "raw_url": "https://github.com/org1/repo1/raw/x/docs/new-name.md",
		 "contents_url": "https://api.github.com/repos/org1/repo1/contents/docs/new-name.md?ref=x",
		 "_links": {"self": "https://api.github.com/repos/org1/repo1/pulls/7/files"}}
	]`))
}

// TestCachedPullFiles_MissAbsorbHit covers the core flow: the first read
// fetches + absorbs the whole trimmed page (miss), the second serves the
// byte-identical stored doc (hit, zero upstream calls), and the rebuild
// drops sha + every URL field while preserving patch/previous_filename
// present-vs-absent exactly.
func TestCachedPullFiles_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/pulls/7/files"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.pullFilesHits))
	assertNoURLKeys(t, w1.Body.Bytes())

	var files []map[string]any
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &files))
	require.Len(t, files, 2)
	first, second := files[0], files[1]

	assert.Equal(t, "internal/api/router.go", first["filename"])
	assert.Equal(t, "modified", first["status"])
	assert.Equal(t, float64(25), first["additions"])
	assert.Equal(t, float64(3), first["deletions"])
	assert.Equal(t, float64(28), first["changes"])
	patch, isString := first["patch"].(string)
	require.True(t, isString, "patch presence must survive (consumers test typeof f.patch === 'string')")
	assert.Equal(t, "@@ -1,3 +1,25 @@ package api", patch)
	_, hasPrev := first["previous_filename"]
	assert.False(t, hasPrev, "previous_filename must stay absent on a non-rename")
	_, hasSHA := first["sha"]
	assert.False(t, hasSHA, "the per-file blob sha is dropped")

	assert.Equal(t, "renamed", second["status"])
	assert.Equal(t, "docs/old-name.md", second["previous_filename"])
	_, hasPatch := second["patch"]
	assert.False(t, hasPatch, "a file GitHub sends without patch must rebuild WITHOUT the key")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.pullFilesHits), "hit must not call upstream")
}

// TestCachedPullFiles_PullRequestEventFlushesOnePR: a PR's own event (here a
// synchronize -- the head moved, so its files changed) flushes exactly that
// PR's cached pages; another PR's pages survive.
func TestCachedPullFiles_PullRequestEventFlushesOnePR(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/files", nil))
	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/files", nil))
	require.Equal(t, int32(2), atomic.LoadInt32(&u.pullFilesHits))

	postWebhook(t, router, "pull_request",
		prEvent("synchronize", upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")))

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/files", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "the PR's own event must flush its files pages")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.pullFilesHits))

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8/files", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "another PR's pages must survive the per-PR flush")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.pullFilesHits))
}

// TestCachedPullFiles_PushEventFlushesRepoWide: a push flushes every PR's
// cached pages for the repo -- the belt for pull_request deliveries we
// missed (a same-repo head push moves some PR's files).
func TestCachedPullFiles_PushEventFlushesRepoWide(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/pulls/7/files"

	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.pullFilesHits))

	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "a push must flush the repo's files pages")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.pullFilesHits))
}

// TestCachedPullFiles_ShapePassthroughs: shapes the cache does not model --
// unknown params, out-of-range paging, and a non-numeric {number} segment
// (GitHub's /pulls/comments/files really exists) -- pass through verbatim,
// uncached, every time.
func TestCachedPullFiles_ShapePassthroughs(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/pulls/7/files?foo=1",        // unknown param
		"/repos/org1/repo1/pulls/7/files?per_page=101", // out of range
		"/repos/org1/repo1/pulls/7/files?page=11",      // beyond the modeled cap
		"/repos/org1/repo1/pulls/comments/files",       // non-numeric {number}
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.pullFilesHits), target)
	}

	// Passthroughs stored nothing: a cacheable shape still misses.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7/files", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
}

// TestCachedPullFiles_DocCapPassthrough: `patch` is unbounded, so a page
// whose rendered document exceeds the 1 MiB cap is passed through verbatim
// -- a passthrough, not an error -- and nothing is stored (every read
// fetches again).
func TestCachedPullFiles_DocCapPassthrough(t *testing.T) {
	router, _, db, u := respCacheStack(t)
	huge := strings.Repeat("x", pullFilesDocMaxBytes+4096)
	u.pullFiles = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"sha": "abc123", "filename": "generated/bundle.js", "status": "modified",
			"additions": 90000, "deletions": 0, "changes": 90000,
			"blob_url": "https://github.com/b", "raw_url": "https://github.com/r",
			"contents_url": "https://api.github.com/c",
			"patch":        huge,
		}})
	}
	target := "/repos/org1/repo1/pulls/7/files"

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader), "an over-cap page must pass through, not error")
		assert.Contains(t, w.Body.String(), "blob_url", "the passthrough is GitHub's verbatim body")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.pullFilesHits), "nothing stored; every read fetches")
	}

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pull_files_cache`).Scan(&count))
	assert.Zero(t, count, "an over-cap page must store no row")
}

// TestCachedPullFiles_EmptyPageCacheable: an empty array -- a page past the
// end of the files list -- is a valid answer for that exact page key and is
// absorbed + served from state like any other.
func TestCachedPullFiles_EmptyPageCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.pullFiles = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[]`))
	}
	target := "/repos/org1/repo1/pulls/7/files?per_page=100&page=3"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a page past the end is a valid cacheable answer")
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.pullFilesHits))
}
