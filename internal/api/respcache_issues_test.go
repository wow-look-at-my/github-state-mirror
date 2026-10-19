package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	issueTarget  = "/repos/org1/repo1/issues/7"
	issuesTarget = "/repos/org1/repo1/issues"
)

func issueObject(number int, title, state, updatedAt string) map[string]any {
	return map[string]any{
		"number":         number,
		"id":             int64(9000000000000000 + number),
		"title":          title,
		"state":          state,
		"updated_at":     updatedAt,
		"comments":       1,
		"url":            "https://api.github.com/repos/org1/repo1/issues/7",
		"html_url":       "https://github.com/org1/repo1/issues/7",
		"comments_url":   "https://api.github.com/repos/org1/repo1/issues/7/comments",
		"user":           map[string]any{"login": "octo", "id": 1, "avatar_url": "https://x"},
		"labels":         []any{map[string]any{"name": "bug", "url": "https://x"}},
		"reactions":      map[string]any{"total_count": 0, "url": "https://x"},
		"closed_by":      nil,
		"pull_request":   nil,
		"timeline_url":   "https://x",
		"repository_url": "https://x",
	}
}

// issuesStack answers both issue routes, counting each.
func issuesStack(t *testing.T) (http.Handler, *int32, *int32, *int, *map[string]any) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var single, list int32
	status := http.StatusOK
	issue := issueObject(7, "first", "open", "2026-10-01T00:00:00Z")
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/7"):
			atomic.AddInt32(&single, 1)
		case strings.HasSuffix(r.URL.Path, "/issues"):
			atomic.AddInt32(&list, 1)
		default:
			return false
		}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(mustJSONString(map[string]any{"message": "Not Found", "status": "404"})))
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/issues") {
			writeGitHubJSON(w, []any{issue})
			return true
		}
		writeGitHubJSON(w, issue)
		return true
	}
	return router, &single, &list, &status, &issue
}

func TestCachedIssue_MissAbsorbHit(t *testing.T) {
	router, single, _, _, _ := issuesStack(t)
	w1 := do(t, router, authedReq("GET", issueTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Contains(t, w1.Body.String(), `"id":9000000000000007`, "an id must stay exact")

	w2 := do(t, router, authedReq("GET", issueTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(single))
}

func TestCachedIssue_NotFoundVerdict(t *testing.T) {
	router, single, _, status, _ := issuesStack(t)
	*status = http.StatusNotFound
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", issueTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code, "request %d", i)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(single))

	// An opened issue drops the verdict; the delivery alone cannot make a row.
	*status = http.StatusOK
	postWebhookJSON(t, router, "issues", map[string]any{
		"action": "opened", "repository": fixtureRepo(),
		"issue": issueObject(7, "first", "open", "2026-10-01T00:00:00Z"),
	})
	w := do(t, router, authedReq("GET", issueTarget, nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
}

func TestCachedIssue_Guards(t *testing.T) {
	router, single, _, _, _ := issuesStack(t)
	for _, target := range []string{issueTarget + "?foo=1", "/repos/org1/repo1/issues/abc"} {
		w := do(t, router, authedReq("GET", target, nil))
		assert.Empty(t, w.Header().Get(cacheHeader), target)
	}
	req := authedReq("GET", issueTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.html+json")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(single), "each passthrough forwards")
}

// An issues delivery rewrites the stored row in place; a comment delivery
// with an older issue view does not regress it.
func TestCachedIssue_PayloadRewritesRow(t *testing.T) {
	router, single, _, _, _ := issuesStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", issueTarget, nil)).Header().Get(cacheHeader))

	edited := issueObject(7, "renamed", "open", "2026-10-02T00:00:00Z")
	postWebhookJSON(t, router, "issues", map[string]any{"action": "edited", "repository": fixtureRepo(), "issue": edited})
	w := do(t, router, authedReq("GET", issueTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Contains(t, w.Body.String(), `"title":"renamed"`)
	assertNoURLKeys(t, w.Body.Bytes())

	stale := issueObject(7, "old", "open", "2026-10-01T12:00:00Z")
	postWebhookJSON(t, router, "issue_comment", map[string]any{
		"action": "created", "repository": fixtureRepo(), "issue": stale,
		"comment": map[string]any{"id": 5, "updated_at": "2026-10-01T12:00:00Z"},
	})
	w = do(t, router, authedReq("GET", issueTarget, nil))
	assert.Contains(t, w.Body.String(), `"title":"renamed"`, "an older issue view must not overwrite a newer one")

	closed := issueObject(7, "renamed", "closed", "2026-10-03T00:00:00Z")
	delete(closed, "closed_by")
	postWebhookJSON(t, router, "issues", map[string]any{"action": "closed", "repository": fixtureRepo(), "issue": closed})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", issueTarget, nil)).Header().Get(cacheHeader),
		"a close without closed_by cannot be applied, so the row is dropped")
	assert.Equal(t, int32(2), atomic.LoadInt32(single))
}

func TestCachedIssuesList_MissHitFlush(t *testing.T) {
	router, _, list, _, _ := issuesStack(t)
	w1 := do(t, router, authedReq("GET", issuesTarget+"?state=open&per_page=30", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())

	// The defaults written out are the same row.
	w2 := do(t, router, authedReq("GET", issuesTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(list))

	assert.Equal(t, "miss", do(t, router, authedReq("GET", issuesTarget+"?state=all&labels=bug", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(list))

	postWebhookJSON(t, router, "issues", map[string]any{
		"action": "opened", "repository": fixtureRepo(), "issue": issueObject(8, "new", "open", "2026-10-05T00:00:00Z"),
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", issuesTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "pull_request", map[string]any{
		"action": "opened", "repository": fixtureRepo(), "number": 9,
		"pull_request": map[string]any{"number": 9, "updated_at": "2026-10-05T00:00:00Z", "state": "open"},
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", issuesTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(5), atomic.LoadInt32(list))
}

func TestCachedIssuesList_Guards(t *testing.T) {
	router, _, list, _, _ := issuesStack(t)
	for _, q := range []string{"?state=bogus", "?page=11", "?sort=updated", "?per_page=0", "?state=open&state=all"} {
		w := do(t, router, authedReq("GET", issuesTarget+q, nil))
		assert.Empty(t, w.Header().Get(cacheHeader), q)
	}
	assert.Equal(t, int32(5), atomic.LoadInt32(list))
}
