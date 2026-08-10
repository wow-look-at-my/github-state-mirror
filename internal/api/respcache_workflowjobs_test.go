package api

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Actions job-route tests (a run's jobs page and a single job); the shared
// fake upstream (respCacheUpstream) lives in respcache_test.go.

const testRunID, testJobID = 9001, 55501

// jobBody renders one GitHub-shaped job object with the given status, full of
// URL fields so the tests can prove the rebuild drops them.
func jobBody(status, conclusion string) string {
	concl := "null"
	if conclusion != "" {
		concl = fmt.Sprintf("%q", conclusion)
	}
	return fmt.Sprintf(`{
		"id": %d, "run_id": %d, "run_attempt": 1,
		"workflow_name": "CI", "head_branch": "main", "head_sha": %q,
		"name": "build", "status": %q, "conclusion": %s,
		"created_at": "2026-08-01T10:00:00Z", "started_at": "2026-08-01T10:00:05Z",
		"completed_at": "2026-08-01T10:04:00Z",
		"labels": ["ubuntu-latest"], "runner_name": "runner-1",
		"url": %q,
		"html_url": %q,
		"run_url": %q,
		"check_run_url": "https://api.github.com/repos/org1/repo1/check-runs/1",
		"node_id": "CR_kwAE",
		"steps": [{"name": "Set up job", "status": %q, "conclusion": %s, "number": 1,
		           "started_at": "2026-08-01T10:00:05Z", "completed_at": "2026-08-01T10:00:07Z"}]
	}`, testJobID, testRunID, shaTip, status, concl,
		fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/jobs/%d", testJobID),
		fmt.Sprintf("https://github.com/org1/repo1/actions/runs/%d/job/%d", testRunID, testJobID),
		fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d", testRunID),
		status, concl)
}

func defaultRunJobsUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{"total_count": 1, "jobs": [%s]}`, jobBody("completed", "success"))
}

func defaultJobUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprint(w, jobBody("completed", "success"))
}

func runJobsTarget() string {
	return fmt.Sprintf("/repos/org1/repo1/actions/runs/%d/jobs", testRunID)
}

func jobTarget() string {
	return fmt.Sprintf("/repos/org1/repo1/actions/jobs/%d", testJobID)
}

// A settled run's jobs page: absorbed on the miss, replayed byte-identically
// on the hit, with every URL field dropped.
func TestCachedRunJobs_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", runJobsTarget(), nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runJobsHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{"total_count": 1, "jobs": [{
		"id": %d, "run_id": %d, "run_attempt": 1,
		"workflow_name": "CI", "head_branch": "main", "head_sha": %q,
		"name": "build", "status": "completed", "conclusion": "success",
		"created_at": "2026-08-01T10:00:00Z", "started_at": "2026-08-01T10:00:05Z",
		"completed_at": "2026-08-01T10:04:00Z",
		"labels": ["ubuntu-latest"], "runner_name": "runner-1",
		"steps": [{"name": "Set up job", "status": "completed", "conclusion": "success",
		           "number": 1, "started_at": "2026-08-01T10:00:05Z",
		           "completed_at": "2026-08-01T10:00:07Z"}]
	}]}`, testJobID, testRunID, shaTip), w1.Body.String())

	w2 := do(t, router, authedReq("GET", runJobsTarget(), nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runJobsHits), "a hit must not call upstream")
}

// The load-bearing rule: a job still running is a LIVE value the runner
// coordinator provisions against. It is never stored, so every read reaches
// GitHub — no TTL, however short, is allowed to answer for it.
func TestCachedRunJobs_LiveJobNeverCached(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.runJobs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"total_count": 1, "jobs": [%s]}`, jobBody("in_progress", ""))
	}

	for i := 1; i <= 3; i++ {
		w := do(t, router, authedReq("GET", runJobsTarget(), nil))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader), "an in-flight job must never be served from the cache")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.runJobsHits), "every read must reach GitHub")
	}

	// Once it finishes, the same read starts caching.
	u.runJobs = defaultRunJobsUpstream
	require.Equal(t, "miss", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader))
}

// One in-flight job poisons the whole page: the page IS the answer, and a
// partially-settled page is a live answer.
func TestCachedRunJobs_MixedPageNotCached(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.runJobs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"total_count": 2, "jobs": [%s, %s]}`,
			jobBody("completed", "success"), jobBody("queued", ""))
	}

	do(t, router, authedReq("GET", runJobsTarget(), nil))
	w := do(t, router, authedReq("GET", runJobsTarget(), nil))
	assert.Empty(t, w.Header().Get(cacheHeader), "one queued job makes the whole page live")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.runJobsHits))
}

// A single completed job caches the same way.
func TestCachedWorkflowJob_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", jobTarget(), nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())

	w2 := do(t, router, authedReq("GET", jobTarget(), nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.jobHits))
}

// A re-run replaces a run's jobs under the SAME run id — so a workflow_job
// delivery naming the run must flush the run's jobs page AND the single-job
// rows under it, which is why both kinds carry run_id.
func TestCachedWorkflowJobs_RerunFlushesRunAndJobRows(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	do(t, router, authedReq("GET", runJobsTarget(), nil))
	do(t, router, authedReq("GET", jobTarget(), nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", jobTarget(), nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "workflow_job", fmt.Sprintf(`{
		"action": "completed",
		"workflow_job": {"id": %d, "run_id": %d, "run_attempt": 2, "name": "build",
		                 "status": "completed", "conclusion": "success", "head_sha": %q},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`, testJobID, testRunID, shaTip))

	assert.Equal(t, "miss", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader),
		"the run's jobs page must be flushed")
	assert.Equal(t, "miss", do(t, router, authedReq("GET", jobTarget(), nil)).Header().Get(cacheHeader),
		"the single-job row under that run must be flushed too")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.runJobsHits))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.jobHits))
}

// A workflow_run delivery carries the same signal (a re-run of the whole run).
func TestCachedWorkflowJobs_WorkflowRunFlushes(t *testing.T) {
	router, _, _, _ := respCacheStack(t)

	do(t, router, authedReq("GET", runJobsTarget(), nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "workflow_run", fmt.Sprintf(`{
		"action": "completed",
		"workflow_run": {"id": %d, "head_sha": %q},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`, testRunID, shaTip))

	assert.Equal(t, "miss", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader))
}

// Shape guards: the `filter` parameter selects a DIFFERENT set of jobs for a
// re-run, deep pages are unmodeled, and non-numeric ids are not this route's.
func TestCachedWorkflowJobs_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, _ := respCacheStack(t)

	for _, tc := range []struct{ name, target, accept string }{
		{"filter parameter", runJobsTarget() + "?filter=all", ""},
		{"deep page", runJobsTarget() + "?page=99", ""},
		{"per_page out of range", runJobsTarget() + "?per_page=500", ""},
		{"query on the single job", jobTarget() + "?x=1", ""},
		{"non-default accept", runJobsTarget(), "application/vnd.github.raw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedReq("GET", tc.target, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			w := do(t, router, req)
			assert.Empty(t, w.Header().Get(cacheHeader), "must be forwarded, not served from the cache")
		})
	}
}
