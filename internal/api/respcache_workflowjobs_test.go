package api

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Actions job-route tests; the shared fake upstream lives in respcache_test.go.

const testRunID, testJobID = 9001, 55501

// jobDoc is GitHub-shaped job object with the given status, full of URL
// fields so the tests can prove the rebuild drops them. An empty conclusion
// means the job has not settled, which the encoder writes as null.
func jobDoc(status, conclusion string) map[string]any {
	var concl any
	if conclusion != "" {
		concl = conclusion
	}
	return map[string]any{
		"id": testJobID, "run_id": testRunID, "run_attempt": 1,
		"workflow_name": "CI", "head_branch": "main", "head_sha": shaTip,
		"name": "build", "status": status, "conclusion": concl,
		"created_at": "2026-08-01T10:00:00Z", "started_at": "2026-08-01T10:00:05Z",
		"completed_at": "2026-08-01T10:04:00Z",
		"labels":       []any{"ubuntu-latest"}, "runner_name": "runner-1",
		"url":           fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/jobs/%d", testJobID),
		"html_url":      fmt.Sprintf("https://github.com/org1/repo1/actions/runs/%d/job/%d", testRunID, testJobID),
		"run_url":       fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d", testRunID),
		"check_run_url": "https://api.github.com/repos/org1/repo1/check-runs/1",
		"node_id":       "CR_kwAE",
		"steps": []any{map[string]any{
			"name": "Set up job", "status": status, "conclusion": concl, "number": 1,
			"started_at": "2026-08-01T10:00:05Z", "completed_at": "2026-08-01T10:00:07Z",
		}},
	}
}

// jobsPage wraps job documents in the runs-jobs listing envelope.
func jobsPage(jobs ...map[string]any) map[string]any {
	items := make([]any, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, j)
	}
	return map[string]any{"total_count": len(items), "jobs": items}
}

func defaultRunJobsUpstream(w http.ResponseWriter, _ *http.Request) {
	writeGitHubJSON(w, jobsPage(jobDoc("completed", "success")))
}

func defaultJobUpstream(w http.ResponseWriter, _ *http.Request) {
	writeGitHubJSON(w, jobDoc("completed", "success"))
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
	assert.JSONEq(t, mustJSONString(map[string]any{"total_count": 1, "jobs": []any{map[string]any{
		"id": testJobID, "run_id": testRunID, "run_attempt": 1,
		"workflow_name": "CI", "head_branch": "main", "head_sha": shaTip,
		"name": "build", "status": "completed", "conclusion": "success",
		"created_at": "2026-08-01T10:00:00Z", "started_at": "2026-08-01T10:00:05Z",
		"completed_at": "2026-08-01T10:04:00Z",
		"labels":       []any{"ubuntu-latest"}, "runner_name": "runner-1",
		"steps": []any{map[string]any{
			"name": "Set up job", "status": "completed", "conclusion": "success",
			"number": 1, "started_at": "2026-08-01T10:00:05Z",
			"completed_at": "2026-08-01T10:00:07Z",
		}},
	}}}), w1.Body.String())

	w2 := do(t, router, authedReq("GET", runJobsTarget(), nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runJobsHits), "a hit must not call upstream")
}

// A RUNNING run is served, and its own deliveries are what keep the answer
// true. This is the load-bearing behavior of the whole route: the fetch
// settles which jobs belong to the run, the workflow_job delivery states the
// new value of of them, and the stored page is rewritten rather than
// dropped — so the runner coordinator reads current job state without the
// fleet re-asking GitHub for what it was already told.
func TestCachedRunJobs_LiveJobIsServedAndRewrittenByDeliveries(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.runJobs = func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, jobsPage(jobDoc("in_progress", "")))
	}

	w1 := do(t, router, authedReq("GET", runJobsTarget(), nil))
	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Contains(t, w1.Body.String(), `"status":"in_progress"`)

	w2 := do(t, router, authedReq("GET", runJobsTarget(), nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a live page is stored, not forwarded")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runJobsHits))

	// The job finishes. GitHub tells us; nothing needs to ask.
	postWebhookJSON(t, router, "workflow_job", map[string]any{
		"action":       "completed",
		"workflow_job": jobDoc("completed", "success"),
		"repository":   fixtureRepo(),
	})

	w3 := do(t, router, authedReq("GET", runJobsTarget(), nil))
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader), "the delivery answered it; nothing to re-fetch")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runJobsHits), "a rewrite must cost no upstream call")
	assert.Contains(t, w3.Body.String(), `"conclusion":"success"`)
	assert.NotContains(t, w3.Body.String(), `"status":"in_progress"`)

	// And the rewritten page is what a fetch would now return, byte for byte.
	u.runJobs = defaultRunJobsUpstream
	fresh := do(t, router, authedReq("GET", runJobsTarget()+"?per_page=100", nil))
	require.Equal(t, "miss", fresh.Header().Get(cacheHeader), "a different page shape is its own row")
	assert.Equal(t, fresh.Body.String(), w3.Body.String(),
		"a rewritten page must be indistinguishable from the fetch it saved")
}

// The live TTL is the LOST-DELIVERY bound, and only that. A page whose
// movement is a job WAITING to start carries it (the deliveries keep such a
// page exactly right, since a queued job has no steps yet); a running job's
// steps advance unreported and earn the shorter; a settled page keeps the
// long. ghdata's TestApplyWorkflowJob_TTLFollowsWhatIsLeftMoving walks all
// .
func TestCachedRunJobs_LiveRowExpiresOnTheShortTTL(t *testing.T) {
	router, _, db, u := respCacheStack(t)
	u.runJobs = func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, jobsPage(jobDoc("completed", "success"), jobDoc("queued", "")))
	}

	do(t, router, authedReq("GET", runJobsTarget(), nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader),
		"one queued job among finished ones is still a page worth serving")

	var expires string
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM workflow_jobs_cache`).Scan(&expires))
	exp, err := time.Parse(time.RFC3339, expires)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(workflowJobsQueuedTTL), exp, 5*time.Second,
		"a page with a queued job carries the live clock, not the settled one")

	_, err = db.Exec(`UPDATE workflow_jobs_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)
	assert.Equal(t, "miss", do(t, router, authedReq("GET", runJobsTarget(), nil)).Header().Get(cacheHeader),
		"past the bound, the next read goes and looks")
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

	postWebhookJSON(t, router, "workflow_job", map[string]any{
		"action": "completed",
		"workflow_job": map[string]any{
			"id": testJobID, "run_id": testRunID, "run_attempt": 2, "name": "build",
			"status": "completed", "conclusion": "success", "head_sha": shaTip,
		},
		"repository": fixtureRepo(),
	})

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

	postWebhookJSON(t, router, "workflow_run", map[string]any{
		"action":       "completed",
		"workflow_run": map[string]any{"id": testRunID, "head_sha": shaTip},
		"repository":   fixtureRepo(),
	})

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
