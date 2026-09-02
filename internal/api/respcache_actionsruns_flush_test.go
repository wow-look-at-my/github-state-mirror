package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What makes a stored runs answer stop being served, and what makes it stop
// NEEDING to be re-fetched: the per-sha webhook flushes, the rewrite a
// `workflow_run` delivery performs instead of, the TTL backstop, and the
// non-200s. The fixtures and the core miss/absorb/hit flow live in
// respcache_actionsruns_test.go.

// TestCachedWorkflowRuns_WebhookFlush: a CI/job event flushes exactly the
// payload-named sha's pages -- including a QUEUED workflow_job, whose
// delivery the dispatcher drops as ignored but whose invalidation runs
// (a queued job is a run the cached listing may not have shown yet, the
// exact staleness pr-minder's zombie probe must not read) -- while another
// sha's pages survive; repository events flush repo-wide.
func TestCachedWorkflowRuns_WebhookFlush(t *testing.T) {
	router, _, _, u := workflowRunsStack(t)
	tipTarget := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip
	midTarget := "/repos/org1/repo1/actions/runs?head_sha=" + shaMid

	seed := func(t *testing.T) {
		t.Helper()
		for _, target := range []string{tipTarget, midTarget} {
			do(t, router, authedReq("GET", target, nil))
			w := do(t, router, authedReq("GET", target, nil))
			require.Equal(t, "hit", w.Header().Get(cacheHeader), "seed must serve: %s", target)
		}
	}

	seed(t)
	postWebhookJSON(t, router, "workflow_job", map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id": 1, "run_id": 9001, "name": "build", "status": "queued", "head_sha": shaTip,
		},
		"repository": fixtureRepo(),
	})
	w := do(t, router, authedReq("GET", tipTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a queued workflow_job must flush its sha's runs pages")
	w = do(t, router, authedReq("GET", midTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "another sha's pages must survive the per-sha flush")

	// A check_run event names its head sha the same way.
	seed(t)
	postWebhookJSON(t, router, "check_run", map[string]any{
		"action": "created",
		"check_run": map[string]any{
			"head_sha": shaTip, "status": "queued", "name": "build",
			"check_suite": map[string]any{"head_branch": "main"},
		},
		"repository": fixtureRepo(),
	})
	w = do(t, router, authedReq("GET", tipTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a check_run event must flush its sha's runs pages")
	w = do(t, router, authedReq("GET", midTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "another sha's pages must survive")

	// repository events flush repo-wide.
	seed(t)
	postWebhook(t, router, "repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	for _, target := range []string{tipTarget, midTarget} {
		w = do(t, router, authedReq("GET", target, nil))
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a repository event must flush every sha's pages: %s", target)
	}
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.otherHits))
}

// A `workflow_run` delivery IS the run object these pages list, so the sha's
// stored pages are rewritten from it rather than dropped. The pin is the same
// as the other rewrites: the result must be what a fetch would now return,
// byte for byte.
//
// A run is UPDATED IN PLACE upstream and the listing is ordered by creation
// (measured: across consecutive runs of repo, created_at is strictly
// descending with violations while updated_at has), so the entry
// cannot move.
func TestCachedWorkflowRuns_WorkflowRunEventRewritesThePage(t *testing.T) {
	router, _, _, u := workflowRunsStack(t)
	target := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip

	page := func(secondStatus string, conclusion, runStartedAt any) map[string]any {
		return map[string]any{"total_count": 3, "workflow_runs": []any{
			upstreamWorkflowRun(9001, "CI", shaTip, "completed", "success", "2026-07-01T10:00:30Z"),
			upstreamWorkflowRun(9002, nil, shaTip, secondStatus, conclusion, runStartedAt),
		}}
	}
	u.runs = func(w http.ResponseWriter, _ *http.Request) { servePRJSON(w, page("queued", nil, nil)) }
	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.runsHits))

	postWebhookJSON(t, router, "workflow_run", map[string]any{
		"action": "completed",
		"workflow_run": upstreamWorkflowRun(9002, nil, shaTip, "completed", "failure",
			"2026-07-01T10:06:00Z"),
		"repository": fixtureRepo(),
	})

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "the delivery answered it; nothing to re-fetch")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runsHits), "a rewrite must cost no upstream call")

	u.runs = func(w http.ResponseWriter, _ *http.Request) {
		servePRJSON(w, page("completed", "failure", "2026-07-01T10:06:00Z"))
	}
	fresh := do(t, router, authedReq("GET", target+"&per_page=50", nil))
	require.Equal(t, "miss", fresh.Header().Get(cacheHeader), "a different page shape is its own row")
	assert.Equal(t, fresh.Body.String(), w.Body.String(),
		"the rewritten page must be byte-identical to the fetch it saved")
}

// A run the pages do not list is a NEW run for the sha -- a membership change
// only a fetch settles -- so that delivery still flushes.
func TestCachedWorkflowRuns_UnlistedRunStillFlushes(t *testing.T) {
	router, _, _, _ := workflowRunsStack(t)
	target := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "workflow_run", map[string]any{
		"action":       "requested",
		"workflow_run": upstreamWorkflowRun(97531, "New", shaTip, "queued", nil, nil),
		"repository":   fixtureRepo(),
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader),
		"a run the page never listed changes the set, which only a fetch settles")
}

// TestCachedWorkflowRuns_TTLBackstopExpiry: even with webhooks silent, a runs
// page expires after its TTL -- the backstop for run DELETION, which emits no
// webhook at all.
func TestCachedWorkflowRuns_TTLBackstopExpiry(t *testing.T) {
	router, _, db, u := workflowRunsStack(t)
	target := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.runsHits))

	_, err := db.Exec(`UPDATE workflow_runs_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired page is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.runsHits))
}

// TestCachedWorkflowRuns_Non200NotStored: anything but a well-formed --
// and a whose body lacks total_count or the workflow_runs array -- is
// relayed verbatim and stores nothing.
func TestCachedWorkflowRuns_Non200NotStored(t *testing.T) {
	router, _, db, u := workflowRunsStack(t)
	target := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip

	u.runs = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"oops"}`))
	}
	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader), "a non-200 must be replayed unstored")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.runsHits))
	}

	// A missing the absorb-gated fields is replayed unstored too.
	u.runs = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, map[string]any{"workflow_runs": []any{}}) // no total_count
	}
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader), "a 200 without total_count must be replayed unstored")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM workflow_runs_cache`).Scan(&count))
	assert.Zero(t, count, "nothing may be stored")
}
