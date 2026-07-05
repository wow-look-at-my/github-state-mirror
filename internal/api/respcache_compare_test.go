package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// upstreamCompare builds a GitHub-shaped compare response: URL clutter at
// every level (permalink/diff/patch URLs, the full base_commit object the
// rebuild drops entirely) around the fields the model absorbs. files == nil
// OMITS the array (GitHub's oversized-response form -- pr-minder reads
// absence as "unknown, fail open"), while an empty non-nil slice yields
// "files": [] (a genuinely 0-diff comparison -- the close-empty gate).
func upstreamCompare(status string, aheadBy, behindBy int, commits []any, files []any) map[string]any {
	web := "https://github.com/org1/repo1/compare/main...dev"
	doc := map[string]any{
		"url":               "https://api.github.com/repos/org1/repo1/compare/main...dev",
		"html_url":          web,
		"permalink_url":     web + "#permalink",
		"diff_url":          web + ".diff",
		"patch_url":         web + ".patch",
		"base_commit":       upstreamCommit(shaBase, "base commit", shaTree1),
		"merge_base_commit": upstreamCommit(shaBase, "base commit", shaTree1),
		"status":            status,
		"ahead_by":          aheadBy,
		"behind_by":         behindBy,
		"total_commits":     len(commits),
		"commits":           commits,
	}
	if files != nil {
		doc["files"] = files
	}
	return doc
}

// upstreamCompareFile builds one GitHub-shaped files entry, including the
// blob/raw/contents URLs and the per-file `patch` the rebuild must drop.
func upstreamCompareFile(filename, status string, additions, deletions int, prev string) map[string]any {
	f := map[string]any{
		"sha":          "f00dfacef00dfacef00dfacef00dfacef00dface",
		"filename":     filename,
		"status":       status,
		"additions":    additions,
		"deletions":    deletions,
		"changes":      additions + deletions,
		"blob_url":     "https://github.com/org1/repo1/blob/main/" + filename,
		"raw_url":      "https://github.com/org1/repo1/raw/main/" + filename,
		"contents_url": "https://api.github.com/repos/org1/repo1/contents/" + filename,
		"patch":        "@@ -1,2 +1,3 @@\n context\n+never stored",
	}
	if prev != "" {
		f["previous_filename"] = prev
	}
	return f
}

// compareCacheUpstream is the fake GitHub for the compare cache tests.
type compareCacheUpstream struct {
	compareHits int32
	probeHits   int32
	compare     func(w http.ResponseWriter, r *http.Request)
	probe       func(w http.ResponseWriter, r *http.Request)
}

func newCompareCacheUpstream() *compareCacheUpstream {
	u := &compareCacheUpstream{}
	u.compare = func(w http.ResponseWriter, r *http.Request) {
		// The commit message carries the requested basehead, so distinct
		// baseheads produce distinguishable trimmed docs.
		basehead := strings.SplitN(r.URL.Path, "/compare/", 2)[1]
		servePRJSON(w, upstreamCompare("ahead", 1, 0,
			[]any{upstreamCommit(shaCommit, "tip of "+basehead, shaTree2, shaBase)},
			[]any{
				upstreamCompareFile("main.go", "modified", 10, 2, ""),
				upstreamCompareFile("renamed.go", "renamed", 0, 0, "old.go"),
			}))
	}
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		// Report a private repo so callers earn grants (like the other fakes).
		servePRJSON(w, map[string]any{
			"name": "repo1", "full_name": "org1/repo1", "private": true, "visibility": "private",
			"html_url": "https://github.com/org1/repo1", "default_branch": "main",
			"owner": map[string]any{"login": "org1"},
		})
	}
	return u
}

func (u *compareCacheUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case r.URL.Path == "/user":
			servePRJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
		case strings.Contains(r.URL.Path, "/compare/"):
			atomic.AddInt32(&u.compareHits, 1)
			u.compare(w, r)
		case len(parts) == 3 && parts[0] == "repos":
			atomic.AddInt32(&u.probeHits, 1)
			u.probe(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
		}
	})
}

func compareCacheStack(t *testing.T) (http.Handler, *ghdata.Store, *sql.DB, *compareCacheUpstream) {
	t.Helper()
	u := newCompareCacheUpstream()
	router, store, db, _ := newTestStackWithGitHub(t, testAuth(), u.handler())
	return router, store, db, u
}

// TestCachedCompare_MissAbsorbHit covers the core flow: the first read fetches
// + absorbs (miss), the second serves the identical trimmed body from state
// (hit, zero upstream calls), the rebuild drops every URL field, the full
// base_commit object, the user-object clutter, and the per-file patch -- and
// the compare's commits land in the SAME global git_commits_cache rows the
// single git-commit route serves.
func TestCachedCompare_MissAbsorbHit(t *testing.T) {
	router, _, _, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"status": "ahead",
		"ahead_by": 1,
		"behind_by": 0,
		"total_commits": 1,
		"merge_base_commit": {"sha": %q},
		"commits": [
			{"sha": %q,
			 "commit": {
				"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-01T10:00:00Z"},
				"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-01T10:05:00Z"},
				"message": "tip of main...dev",
				"tree": {"sha": %q}},
			 "parents": [{"sha": %q}]}
		],
		"files": [
			{"filename": "main.go", "status": "modified", "additions": 10, "deletions": 2, "changes": 12},
			{"filename": "renamed.go", "status": "renamed", "additions": 0, "deletions": 0, "changes": 0,
			 "previous_filename": "old.go"}
		]
	}`, shaBase, shaCommit, shaTree2, shaBase), w1.Body.String())
	assert.NotContains(t, w1.Body.String(), "never stored", "the per-file patch must be dropped")
	assert.NotContains(t, w1.Body.String(), "base commit", "the base_commit object must be dropped")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits), "hit must not call upstream")

	// Absorb synergy: the compare's commits are global git_commits_cache rows,
	// so the single git-commit route hits without its own fetch ever having
	// happened (this fake 404s that endpoint -- a miss could not serve).
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaCommit, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader), "compare-absorbed commits must serve the single git-commit route")
}

// TestCachedCompare_ChangedFilesPreserved pins the field whose loss got the
// old /compare cache REMOVED: pr-minder reads changed_files = files.length,
// where an EMPTY array means a 0-diff branch (close/skip -- the empty-PR
// gate) and an ABSENT array means unknown (fail open, never close). Both
// forms must survive the rebuild exactly, on the miss and on the hit.
func TestCachedCompare_ChangedFilesPreserved(t *testing.T) {
	router, _, _, u := compareCacheStack(t)

	filesOf := func(t *testing.T, body []byte) ([]any, bool) {
		t.Helper()
		var doc map[string]any
		require.NoError(t, json.Unmarshal(body, &doc))
		v, present := doc["files"]
		if !present {
			return nil, false
		}
		arr, ok := v.([]any)
		require.True(t, ok, "files must be an array when present: %s", body)
		return arr, true
	}

	// A squash-merge orphan: ahead by commit count, ZERO net diff. files must
	// rebuild as a present-and-empty array so the close-empty gate fires.
	u.compare = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, upstreamCompare("ahead", 2, 0,
			[]any{upstreamCommit(shaCommit, "squashed elsewhere", shaTree2, shaBase)}, []any{}))
	}
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/main...orphan", nil))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, want, w.Header().Get(cacheHeader))
		files, present := filesOf(t, w.Body.Bytes())
		assert.True(t, present, "an empty files array must stay PRESENT (round %d)", i+1)
		assert.Empty(t, files, "a 0-diff comparison must rebuild files as [] (round %d)", i+1)
	}

	// An oversized comparison: GitHub OMITS the files array. The rebuild must
	// omit it too -- inventing an empty one would flip "unknown" into "0-diff"
	// and close a real PR.
	u.compare = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, upstreamCompare("ahead", 3, 0,
			[]any{upstreamCommit(shaCommit, "huge diff", shaTree2, shaBase)}, nil))
	}
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/main...huge", nil))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, want, w.Header().Get(cacheHeader))
		_, present := filesOf(t, w.Body.Bytes())
		assert.False(t, present, "an omitted files array must stay ABSENT (round %d)", i+1)
	}

	// An N-file comparison keeps its exact length and per-file counts.
	u.compare = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, upstreamCompare("ahead", 1, 0,
			[]any{upstreamCommit(shaCommit, "three files", shaTree2, shaBase)},
			[]any{
				upstreamCompareFile("a.go", "added", 5, 0, ""),
				upstreamCompareFile("b.go", "modified", 1, 1, ""),
				upstreamCompareFile("c.go", "removed", 0, 9, ""),
			}))
	}
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/main...triple", nil))
	require.Equal(t, http.StatusOK, w.Code)
	files, present := filesOf(t, w.Body.Bytes())
	require.True(t, present)
	require.Len(t, files, 3, "changed_files = files.length must survive the trim")
	first, ok := files[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a.go", first["filename"])
	assert.Equal(t, float64(5), first["additions"])
	assert.Equal(t, float64(0), first["deletions"])
	assert.Equal(t, float64(5), first["changes"])
	assert.NotContains(t, first, "previous_filename", "previous_filename is omitted when GitHub sent none")
}

// TestCachedCompare_BaseheadKeying: every distinct basehead is its own row --
// including the reversed pair -- and branch names with slashes route into the
// cached route (the greedy wildcard), not past it.
func TestCachedCompare_BaseheadKeying(t *testing.T) {
	router, _, _, u := compareCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/main...dev", nil))
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/dev...main", nil))
	require.Equal(t, "miss", w2.Header().Get(cacheHeader), "the reversed pair is its own key")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.compareHits))
	assert.NotEqual(t, w1.Body.String(), w2.Body.String())

	// Slashed branch names on both sides still match the greedy route.
	slashed := "/repos/org1/repo1/compare/claude/my-branch...release/v2.0"
	w3 := do(t, router, authedReq("GET", slashed, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "slashed baseheads must reach the cached route")
	assert.Contains(t, w3.Body.String(), "tip of claude/my-branch...release/v2.0")

	// Each key serves from its own row.
	for _, tc := range []struct{ target, body string }{
		{"/repos/org1/repo1/compare/main...dev", w1.Body.String()},
		{"/repos/org1/repo1/compare/dev...main", w2.Body.String()},
		{slashed, w3.Body.String()},
	} {
		w := do(t, router, authedReq("GET", tc.target, nil))
		assert.Equal(t, "hit", w.Header().Get(cacheHeader), tc.target)
		assert.Equal(t, tc.body, w.Body.String(), tc.target)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.compareHits))
}

// TestCachedCompare_PassthroughShapes: shapes the cache does not model pass
// through verbatim, uncached, every time -- query params, the diff/patch
// media types, the cross-fork owner:branch form (whose fork side this repo's
// webhooks can never invalidate), and a basehead without the three-dot
// separator.
func TestCachedCompare_PassthroughShapes(t *testing.T) {
	router, _, _, u := compareCacheStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/compare/main...dev?per_page=10", // query params are not modeled
		"/repos/org1/repo1/compare/main...dev?page=2",      // ...none of them
		"/repos/org1/repo1/compare/main...other:branch",    // cross-fork head
		"/repos/org1/repo1/compare/other:main...dev",       // cross-fork base
		"/repos/org1/repo1/compare/main..dev",              // two-dot form (no ...)
		"/repos/org1/repo1/compare/just-one-ref",           // no separator at all
		"/repos/org1/repo1/compare/...dev",                 // empty base
		"/repos/org1/repo1/compare/main...",                // empty head
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.compareHits), target)
	}

	// A non-default Accept (the diff media type) passes through too.
	req := authedReq("GET", "/repos/org1/repo1/compare/main...dev", nil)
	req.Header.Set("Accept", "application/vnd.github.diff")
	w := do(t, router, req)
	assert.Empty(t, w.Header().Get(cacheHeader), "non-default Accept must pass through")
	assert.Equal(t, int32(9), atomic.LoadInt32(&u.compareHits))

	// Passthroughs stored nothing: a cacheable shape still misses.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/main...dev", nil))
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader))
}

// TestCachedCompare_WebhookFlush: push AND repository events flush the repo's
// compare docs (either side of any basehead may have moved) while the
// absorbed git-commit rows -- immutable -- survive.
func TestCachedCompare_WebhookFlush(t *testing.T) {
	router, _, db, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))

	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "push must flush the compare doc")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.compareHits))

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w3.Header().Get(cacheHeader), "the re-absorbed doc serves again")

	postWebhook(t, router, "repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w4 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader), "repository events must flush the compare doc too")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.compareHits))

	// The absorbed commit rows are immutable global truth and survive flushes.
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM git_commits_cache`).Scan(&count))
	assert.Greater(t, count, 0, "git-commit rows must survive the compare flush")
}

// TestCachedCompare_TTLBackstopExpiry: even with webhooks silent, a compare
// doc expires after its TTL -- a missed push can't serve a stale comparison
// forever.
func TestCachedCompare_TTLBackstopExpiry(t *testing.T) {
	router, _, db, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))

	_, err := db.Exec(`UPDATE compare_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired doc is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.compareHits))
}

// TestCachedCompare_Non200NotStored: 404 (unknown ref -- it can be pushed
// later), 5xx -- anything but a 200 -- is relayed verbatim and stores nothing.
func TestCachedCompare_Non200NotStored(t *testing.T) {
	router, _, db, u := compareCacheStack(t)
	u.compare = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/main...ghostbranch", nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader), "a non-200 must be replayed unstored")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.compareHits))
	}

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM compare_cache`).Scan(&count))
	assert.Zero(t, count, "a non-200 answer must store no compare doc")
}

// TestCachedCompare_RevealDenied: an unauthorized caller gets GitHub's own
// relayed denial and never reaches the compare fetch; the repeat request is
// answered from the deny cache without touching GitHub.
func TestCachedCompare_RevealDenied(t *testing.T) {
	router, _, _, u := compareCacheStack(t)
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	}
	target := "/repos/org1/ghost/compare/main...dev"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader), "a fresh probe denial is a miss")
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.compareHits), "a denied caller never reaches the compare fetch")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a cached deny verdict answers without GitHub")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.compareHits))
}

// TestCompareBaseheadCacheable pins the shape gate: the consumers' three-dot
// form (slashes and all) is cacheable; anything else is not.
func TestCompareBaseheadCacheable(t *testing.T) {
	for _, good := range []string{
		"main...dev",
		"claude/my-branch...release/v2.0",
		"main...claude/zen-volta-ldcyx6",
	} {
		assert.True(t, compareBaseheadCacheable(good), good)
	}
	for _, bad := range []string{
		"", "main", "main..dev", "...dev", "main...", "...",
		"main...other:branch", "other:main...dev", "owner:main...owner:dev",
	} {
		assert.False(t, compareBaseheadCacheable(bad), bad)
	}
}
