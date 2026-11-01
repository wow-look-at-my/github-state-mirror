package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Single commit and branches-where-head tests.

const (
	commitByShaTarget    = "/repos/org1/repo1/commits/" + shaTip
	commitByBranchTarget = "/repos/org1/repo1/commits/main"
	whereHeadTarget      = "/repos/org1/repo1/commits/" + shaTip + "/branches-where-head"
)

func commitObject() map[string]any {
	person := map[string]any{"name": "Alice", "email": "a@example.com", "date": "2026-07-01T10:00:00Z"}
	return map[string]any{
		"sha": shaTip, "node_id": "C_kw", "url": "https://api.github.com/x", "html_url": "https://github.com/x",
		"comments_url": "https://api.github.com/x",
		"commit": map[string]any{
			"author": person, "committer": person, "message": "fix", "comment_count": 0,
			"url":          "https://api.github.com/x",
			"tree":         map[string]any{"sha": shaTree1, "url": "https://api.github.com/x"},
			"verification": map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil, "verified_at": nil},
		},
		"author": nil, "committer": map[string]any{"login": "web-flow", "id": 19864447, "type": "User", "url": "https://api.github.com/x"},
		"parents": []any{map[string]any{"sha": shaBase, "url": "https://api.github.com/x", "html_url": "https://github.com/x"}},
		"stats":   map[string]any{"additions": 3, "deletions": 1, "total": 4},
		"files": []any{
			map[string]any{"sha": shaTree1, "filename": "a.go", "status": "modified", "additions": 3, "deletions": 1, "changes": 4,
				"patch": "@@ -1 +1 @@", "blob_url": "https://github.com/x", "raw_url": "https://github.com/x", "contents_url": "https://api.github.com/x"},
			map[string]any{"sha": shaTree1, "filename": "b.bin", "status": "renamed", "additions": 0, "deletions": 0, "changes": 0,
				"previous_filename": "a.bin"},
		},
	}
}

func trimmedCommit() map[string]any {
	person := map[string]any{"name": "Alice", "email": "a@example.com", "date": "2026-07-01T10:00:00Z"}
	return map[string]any{
		"sha": shaTip, "node_id": "C_kw",
		"commit": map[string]any{
			"author": person, "committer": person, "message": "fix", "comment_count": 0,
			"tree":         map[string]any{"sha": shaTree1},
			"verification": map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil, "verified_at": nil},
		},
		"author": nil, "committer": map[string]any{"login": "web-flow", "id": 19864447, "type": "User"},
		"parents": []any{map[string]any{"sha": shaBase}},
		"stats":   map[string]any{"additions": 3, "deletions": 1, "total": 4},
		"files": []any{
			map[string]any{"sha": shaTree1, "filename": "a.go", "status": "modified", "additions": 3, "deletions": 1, "changes": 4, "patch": "@@ -1 +1 @@"},
			map[string]any{"sha": shaTree1, "filename": "b.bin", "status": "renamed", "additions": 0, "deletions": 0, "changes": 0, "previous_filename": "a.bin"},
		},
	}
}

// commitStack answers the commit and branches-where-head reads.
func commitStack(t *testing.T) (http.Handler, *int32, *int32, *int) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var commitCalls, headCalls int32
	status := http.StatusOK
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		tail, ok := strings.CutPrefix(r.URL.Path, "/repos/org1/repo1/commits/")
		if !ok {
			return false
		}
		if strings.HasSuffix(tail, "/branches-where-head") {
			atomic.AddInt32(&headCalls, 1)
			writeGitHubJSON(w, []any{map[string]any{
				"name": "main", "protected": true,
				"commit": map[string]any{"sha": shaTip, "url": "https://api.github.com/x"},
			}})
			return true
		}
		atomic.AddInt32(&commitCalls, 1)
		if status == http.StatusOK && r.Header.Get("Accept") == "application/vnd.github.diff" {
			w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
			_, _ = w.Write([]byte(commitDiffBody))
			return true
		}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"No commit found for SHA: nope","status":"422"}`))
			return true
		}
		writeGitHubJSON(w, commitObject())
		return true
	}
	return router, &commitCalls, &headCalls, &status
}

func TestCachedCommit_MissAbsorbHit(t *testing.T) {
	router, calls, _, _ := commitStack(t)

	w1 := do(t, router, authedReq("GET", commitByShaTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString(trimmedCommit()), w1.Body.String())

	w2 := do(t, router, authedReq("GET", commitByShaTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// A branch spelling describes whatever the branch points at, so a push to it
// drops the row; a full-sha row is immutable and stays.
func TestCachedCommit_PushFlushesRefSpellingsOnly(t *testing.T) {
	router, calls, _, _ := commitStack(t)
	for _, target := range []string{commitByShaTarget, commitByBranchTarget} {
		require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	}
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip, "repository": fixtureRepo(), "commits": []any{},
	})
	assert.Equal(t, "hit", do(t, router, authedReq("GET", commitByShaTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", commitByBranchTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
}

func TestCachedCommit_UnknownRefVerdict(t *testing.T) {
	router, calls, _, status := commitStack(t)
	*status = http.StatusUnprocessableEntity
	target := "/repos/org1/repo1/commits/nope"
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestCachedCommit_ShapeGuardsPassThrough(t *testing.T) {
	router, calls, _, _ := commitStack(t)
	req := authedReq("GET", commitByShaTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.patch")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Empty(t, do(t, router, authedReq("GET", commitByShaTarget+"?page=2", nil)).Header().Get(cacheHeader))
	assert.Empty(t, do(t, router, authedReq("GET", "/repos/org1/repo1/commits/feature/x", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
}

// A push moving main OFF the sha removes it from the sha's answer in place.
// A push moving a branch ONTO the sha drops the answer, because the entry's
// protected flag is not in the payload.
func TestCachedBranchesWhereHead_PushApplies(t *testing.T) {
	router, _, calls, _ := commitStack(t)

	w1 := do(t, router, authedReq("GET", whereHeadTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString([]any{map[string]any{
		"name": "main", "commit": map[string]any{"sha": shaTip}, "protected": true,
	}}), w1.Body.String())

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaTip, "after": shaBase, "repository": fixtureRepo(), "commits": []any{},
	})
	w2 := do(t, router, authedReq("GET", whereHeadTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/dev", "before": shaBase, "after": shaTip, "repository": fixtureRepo(), "commits": []any{},
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", whereHeadTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestCachedBranchesWhereHead_DeleteRemovesTheBranch(t *testing.T) {
	router, _, calls, _ := commitStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", whereHeadTarget, nil)).Header().Get(cacheHeader))
	postWebhookJSON(t, router, "delete", map[string]any{"ref": "main", "ref_type": "branch", "repository": fixtureRepo()})
	w := do(t, router, authedReq("GET", whereHeadTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}
