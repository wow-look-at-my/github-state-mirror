package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const commitDiffBody = "diff --git a/a.go b/a.go\n+x\n"

func readCommitDiff(t *testing.T, router http.Handler, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := authedReq("GET", target, nil)
	req.Header.Set("Accept", accept)
	return do(t, router, req)
}

// The diff is stored verbatim beside the JSON commit, and both diff spellings share one row.
func TestCachedCommitDiff_MissHit(t *testing.T) {
	router, calls, _, _ := commitStack(t)
	for i, tc := range []struct{ accept, want string }{
		{"application/vnd.github.diff", "miss"},
		{"application/vnd.github.v3.diff", "hit"},
	} {
		w := readCommitDiff(t, router, commitByShaTarget, tc.accept)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, commitDiffBody, w.Body.String(), "request %d", i)
		assert.Equal(t, "application/vnd.github.diff; charset=utf-8", w.Header().Get("Content-Type"))
		assert.Equal(t, tc.want, w.Header().Get(cacheHeader), "request %d", i)
	}
	// The JSON commit is a different row.
	w := do(t, router, authedReq("GET", commitByShaTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
	assertNoURLKeys(t, w.Body.Bytes())
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

// A branch-spelled diff follows the commit kind's flush: a push to the branch drops it.
func TestCachedCommitDiff_PushFlushesBranchSpelling(t *testing.T) {
	router, calls, _, _ := commitStack(t)
	for _, target := range []string{commitByShaTarget, commitByBranchTarget} {
		require.Equal(t, "miss", readCommitDiff(t, router, target, "application/vnd.github.diff").Header().Get(cacheHeader))
	}
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip, "repository": fixtureRepo(), "commits": []any{},
	})
	assert.Equal(t, "hit", readCommitDiff(t, router, commitByShaTarget, "application/vnd.github.diff").Header().Get(cacheHeader))
	assert.Equal(t, "miss", readCommitDiff(t, router, commitByBranchTarget, "application/vnd.github.diff").Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
}

func TestCachedCommitDiff_UnknownRefVerdict(t *testing.T) {
	router, calls, _, status := commitStack(t)
	*status = http.StatusUnprocessableEntity
	for i, want := range []string{"miss", "hit"} {
		w := readCommitDiff(t, router, "/repos/org1/repo1/commits/b539f55", "application/vnd.github.diff")
		require.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// A multi-range Accept is not modeled and keeps the passthrough.
func TestCachedCommitDiff_MixedAcceptPassesThrough(t *testing.T) {
	router, calls, _, _ := commitStack(t)
	w := readCommitDiff(t, router, commitByShaTarget, "application/vnd.github.diff, application/json")
	assert.Empty(t, w.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}
