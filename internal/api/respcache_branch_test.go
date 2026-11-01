package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Single-branch read tests.

func branchObject(name, sha string) map[string]any {
	return map[string]any{
		"name": name,
		"commit": map[string]any{
			"sha": sha,
			"url": "https://api.github.com/repos/org1/repo1/commits/" + sha,
			"commit": map[string]any{
				"author":    map[string]any{"name": "a", "email": "a@example.com", "date": "2026-10-01T00:00:00Z"},
				"committer": map[string]any{"name": "c", "email": "c@example.com", "date": "2026-10-01T00:00:00Z"},
				"message":   "msg",
				"tree":      map[string]any{"sha": shaTree1, "url": "https://api.github.com/repos/org1/repo1/git/trees/" + shaTree1},
				"url":       "https://api.github.com/repos/org1/repo1/git/commits/" + sha,
			},
			"parents":  []any{map[string]any{"sha": shaBase, "url": "https://api.github.com/x", "html_url": "https://github.com/x"}},
			"html_url": "https://github.com/org1/repo1/commit/" + sha,
		},
		"_links":         map[string]any{"self": "https://api.github.com/x"},
		"protected":      true,
		"protection_url": "https://api.github.com/x/protection",
	}
}

// branchStack answers the single-branch read, counting calls.
func branchStack(t *testing.T) (http.Handler, *int32, map[string]string) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var calls int32
	branches := map[string]string{"main": shaTip}
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		name, ok := strings.CutPrefix(r.URL.Path, "/repos/org1/repo1/branches/")
		if !ok {
			return false
		}
		atomic.AddInt32(&calls, 1)
		sha, ok := branches[name]
		if !ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Branch not found","documentation_url":"https://docs.github.com","status":"404"}`))
			return true
		}
		writeGitHubJSON(w, branchObject(name, sha))
		return true
	}
	return router, &calls, branches
}

func TestCachedBranch_MissAbsorbHit(t *testing.T) {
	router, calls, _ := branchStack(t)

	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/branches/main", nil))
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Contains(t, w1.Body.String(), `"sha":"`+shaTip+`"`)
	assert.Contains(t, w1.Body.String(), `"protected":true`)

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/branches/main", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// The observed traffic: a branch that does not exist. The verdict is stored,
// and a create of that branch drops it.
func TestCachedBranch_AbsentVerdictFlushedByCreate(t *testing.T) {
	router, calls, branches := branchStack(t)
	target := "/repos/org1/repo1/branches/claude/x"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	branches["claude/x"] = shaMid
	postWebhookJSON(t, router, "create", map[string]any{
		"ref": "claude/x", "ref_type": "branch", "repository": fixtureRepo(),
	})
	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

// chi routes on the raw path, so an encoded slash keys the same row.
func TestCachedBranch_EncodedSlashSharesRow(t *testing.T) {
	router, calls, branches := branchStack(t)
	branches["feature/a"] = shaMid

	require.Equal(t, "miss", do(t, router, authedReq("GET", "/repos/org1/repo1/branches/feature/a", nil)).Header().Get(cacheHeader))
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/branches/feature%2Fa", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// A push to the branch drops its row; a push to another branch does not.
func TestCachedBranch_PushFlushesOnlyThatBranch(t *testing.T) {
	router, calls, branches := branchStack(t)
	branches["dev"] = shaMid

	do(t, router, authedReq("GET", "/repos/org1/repo1/branches/main", nil))
	do(t, router, authedReq("GET", "/repos/org1/repo1/branches/dev", nil))
	require.Equal(t, int32(2), atomic.LoadInt32(calls))

	branches["main"] = shaCommit
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaTip, "after": shaCommit,
		"repository": fixtureRepo(),
	})
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/branches/main", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
	assert.Contains(t, w.Body.String(), shaCommit)
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/branches/dev", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
}

func TestCachedBranch_ShapeGuards(t *testing.T) {
	router, calls, _ := branchStack(t)

	assert.Empty(t, do(t, router, authedReq("GET", "/repos/org1/repo1/branches/main?x=1", nil)).Header().Get(cacheHeader))
	req := authedReq("GET", "/repos/org1/repo1/branches/main", nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}
