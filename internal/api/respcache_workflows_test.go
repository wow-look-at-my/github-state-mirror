package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Actions workflow-definition route tests. The default bodies carry url and
// badge_url so the rebuild has something to drop, and html_url so the pinned
// exception is exercised.

const (
	workflowsListTarget  = "/repos/org1/repo1/actions/workflows"
	workflowSingleTarget = workflowsListTarget + "/161335"
	workflowSingleByFile = workflowsListTarget + "/ci.yml"
	workflowsAllowedURLs = "html_url"
)

// trimmedWorkflow is the rebuild the route must serve: url and badge_url gone,
// html_url kept as the pinned exception.
func trimmedWorkflow() map[string]any {
	return map[string]any{
		"id": 161335, "node_id": "MDg6V29ya2Zsb3cxNjEzMzU=", "name": "CI",
		"path": ".github/workflows/ci.yml", "state": "active",
		"created_at": "2026-01-01T10:00:00.000-08:00",
		"updated_at": "2026-01-02T10:00:00.000-08:00",
		"html_url":   "https://github.com/org1/repo1/blob/main/.github/workflows/ci.yml",
	}
}

func workflowObject() map[string]any {
	return map[string]any{
		"id": 161335, "node_id": "MDg6V29ya2Zsb3cxNjEzMzU=", "name": "CI",
		"path": ".github/workflows/ci.yml", "state": "active",
		"created_at": "2026-01-01T10:00:00.000-08:00",
		"updated_at": "2026-01-02T10:00:00.000-08:00",
		"url":        "https://api.github.com/repos/org1/repo1/actions/workflows/161335",
		"html_url":   "https://github.com/org1/repo1/blob/main/.github/workflows/ci.yml",
		"badge_url":  "https://github.com/org1/repo1/workflows/CI/badge.svg",
	}
}

func defaultWorkflowsUpstream(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/actions/workflows") {
		writeGitHubJSON(w, map[string]any{
			"total_count": 1, "workflows": []any{workflowObject()},
		})
		return
	}
	writeGitHubJSON(w, workflowObject())
}

func TestCachedWorkflow_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", workflowSingleTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes(), workflowsAllowedURLs)
	assert.JSONEq(t, mustJSONString(trimmedWorkflow()), w1.Body.String())

	w2 := do(t, router, authedReq("GET", workflowSingleTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.workflowsHits), "a hit must not call upstream")
}

func TestCachedWorkflowsList_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", workflowsListTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes(), workflowsAllowedURLs)
	assert.JSONEq(t, mustJSONString(map[string]any{
		"total_count": 1, "workflows": []any{trimmedWorkflow()},
	}), w1.Body.String())

	w2 := do(t, router, authedReq("GET", workflowsListTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.workflowsHits))
}

// The listing and the single read are separate grains, and a file-name
// spelling is its own row: resolving it to the numeric id needs this answer.
func TestCachedWorkflows_GrainsAndSpellingsAreDistinctRows(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", workflowsListTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "miss", do(t, router, authedReq("GET", workflowSingleTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "miss", do(t, router, authedReq("GET", workflowSingleByFile, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.workflowsHits))

	require.Equal(t, "hit", do(t, router, authedReq("GET", workflowSingleByFile, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.workflowsHits))
}

// A push is what edits .github/workflows, so it flushes both grains repo-wide.
func TestCachedWorkflows_PushFlushesBothGrains(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", workflowsListTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "miss", do(t, router, authedReq("GET", workflowSingleTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, int32(2), atomic.LoadInt32(&u.workflowsHits))

	// A push to a branch that is not the default: a workflow file edit counts wherever it lands.
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/feature", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(), "commits": []any{},
	})

	assert.Equal(t, "miss", do(t, router, authedReq("GET", workflowsListTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", workflowSingleTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.workflowsHits))
}

// An empty listing is the valid, cacheable "this repo has no workflows".
func TestCachedWorkflowsList_EmptyIsCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.workflows = func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, map[string]any{"total_count": 0, "workflows": []any{}})
	}

	w1 := do(t, router, authedReq("GET", workflowsListTarget, nil))
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"total_count":0,"workflows":[]}`, w1.Body.String())

	assert.Equal(t, "hit", do(t, router, authedReq("GET", workflowsListTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.workflowsHits))
}

// Shape guards: anything the route does not model forwards with a counted
// reason instead of vanishing.
func TestCachedWorkflows_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// An unmodeled listing parameter.
	w := do(t, router, authedReq("GET", workflowsListTarget+"?created=today", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// The single read takes no parameters at all.
	w = do(t, router, authedReq("GET", workflowSingleTarget+"?per_page=1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// A non-default Accept.
	req := authedReq("GET", workflowsListTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w = do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// A deeper path (a workflow's runs, its dispatches) is not claimed here.
	w = do(t, router, authedReq("GET", workflowSingleTarget+"/runs", nil))
	assert.Empty(t, w.Header().Get(cacheHeader))

	assert.Equal(t, int32(4), atomic.LoadInt32(&u.workflowsHits), "all must have reached GitHub")
}

// A deleted workflow relays verbatim and stores nothing, so the next read still
// reaches GitHub rather than replaying a hole.
func TestCachedWorkflow_AbsentIsNeverStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.workflows = absentReadmeUpstream

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", workflowSingleTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.workflowsHits))
	}
}
