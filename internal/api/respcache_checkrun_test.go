package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Single check-run route tests.

const checkRunTarget = "/repos/org1/repo1/check-runs/555"

func defaultCheckRunUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, map[string]any{
		"id": 555, "head_sha": shaBase, "name": "build", "status": "completed",
		"conclusion": "success", "started_at": "2026-08-01T00:00:00Z", "completed_at": "2026-08-01T00:05:00Z",
		"app":         map[string]any{"id": 42, "slug": "gha", "html_url": "https://github.com/apps/gha"},
		"output":      map[string]any{"title": "All checks passed", "summary": "long text", "text": "even longer"},
		"details_url": "https://ci.example.com/build/555", "html_url": "https://github.com/org1/repo1/runs/555",
		"url":     "https://api.github.com" + r.URL.Path,
		"node_id": "CR_kwAE", "check_suite": map[string]any{"id": 999},
		"pull_requests": []any{},
	})
}

func TestCachedCheckRun_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", checkRunTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunHits))
	assert.NotContains(t, w1.Body.String(), "api.github.com")
	assert.NotContains(t, w1.Body.String(), "node_id")
	assert.NotContains(t, w1.Body.String(), "check_suite")
	assert.NotContains(t, w1.Body.String(), "pull_requests")
	// output is trimmed to its title (the pinned consumer contract).
	assert.Contains(t, w1.Body.String(), `"title":"All checks passed"`)
	assert.NotContains(t, w1.Body.String(), "long text")
	// details_url/html_url are the pinned consumer-read exception.
	assert.Contains(t, w1.Body.String(), `"details_url":"https://ci.example.com/build/555"`)

	w2 := do(t, router, authedReq("GET", checkRunTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunHits), "a hit must not call upstream")
}

// The load-bearing property: a `check_run` delivery rewrites the row IN
// PLACE from its own object, so a re-read after the delivery must see the
// NEW state without any further upstream call (APPLY THE PAYLOAD).
func TestCachedCheckRun_WebhookRewritesInPlace(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", checkRunTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", checkRunTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "check_run", map[string]any{
		"action": "completed",
		"check_run": map[string]any{
			"id": 555, "head_sha": shaBase, "name": "build", "status": "completed",
			"conclusion": "failure", "started_at": "2026-08-01T00:00:00Z", "completed_at": "2026-08-01T00:06:00Z",
			"app":    map[string]any{"id": 42},
			"output": map[string]any{"title": "1 check failed"},
		},
		"repository": fixtureRepo(),
	})

	w := do(t, router, authedReq("GET", checkRunTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "the delivery rewrote the row; this must still be a hit")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunHits), "a rewrite-in-place must never re-fetch")
	assert.Contains(t, w.Body.String(), `"conclusion":"failure"`)
	assert.Contains(t, w.Body.String(), `"title":"1 check failed"`)
}

// A repository event (rename/delete) has no run id to rewrite with, so it
// falls back to the repo-wide flush.
func TestCachedCheckRun_RepositoryEventFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", checkRunTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", checkRunTarget, nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "repository", `{"action":"renamed","repository":{"name":"repo1","owner":{"login":"org1"},"private":true,"visibility":"private","default_branch":"main","full_name":"org1/repo1"}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", checkRunTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.checkRunHits))
}

// Shape guards: a non-numeric id, a query parameter, and a non-default
// Accept all forward with a counted reason.
func TestCachedCheckRun_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	for _, target := range []string{
		"/repos/org1/repo1/check-runs/not-a-number",
		checkRunTarget + "?filter=latest",
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "%s must forward", target)
	}

	req := authedReq("GET", checkRunTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.checkRunHits), "every unmodeled shape forwards to GitHub uncached")
}

// A / relays unstored, never a cached verdict -- GitHub authorization
// on a check run can change with no event reaching the mirror.
func TestCachedCheckRun_NotFoundRelayedUnstored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.checkRun = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", checkRunTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.checkRunHits))
	}
}
