package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// upstreamWorkflowRun builds one GitHub-shaped workflow_runs entry: the full
// actor/repository/head_commit objects and every *_url the rebuild must drop
// around the fields the model keeps. name/conclusion/runStartedAt may be nil.
func upstreamWorkflowRun(id int, name any, headSHA, status string, conclusion, runStartedAt any) map[string]any {
	return map[string]any{
		"id":                   id,
		"name":                 name,
		"node_id":              "WFR_xyz",
		"head_branch":          "main",
		"head_sha":             headSHA,
		"path":                 ".github/workflows/ci.yml",
		"run_number":           7,
		"event":                "push",
		"status":               status,
		"conclusion":           conclusion,
		"workflow_id":          42,
		"check_suite_id":       5,
		"url":                  fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d", id),
		"html_url":             fmt.Sprintf("https://github.com/org1/repo1/actions/runs/%d", id),
		"jobs_url":             fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d/jobs", id),
		"logs_url":             fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d/logs", id),
		"cancel_url":           fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d/cancel", id),
		"rerun_url":            fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d/rerun", id),
		"artifacts_url":        fmt.Sprintf("https://api.github.com/repos/org1/repo1/actions/runs/%d/artifacts", id),
		"workflow_url":         "https://api.github.com/repos/org1/repo1/actions/workflows/42",
		"created_at":           "2026-07-01T10:00:00Z",
		"updated_at":           "2026-07-01T10:05:00Z",
		"run_started_at":       runStartedAt,
		"pull_requests":        []any{},
		"referenced_workflows": []any{},
		"actor": map[string]any{
			"login": "octocat", "id": 1, "type": "User",
			"avatar_url": "https://avatars.githubusercontent.com/u/1",
			"url":        "https://api.github.com/users/octocat",
		},
		"repository": map[string]any{
			"id": 1, "name": "repo1", "full_name": "org1/repo1",
			"url": "https://api.github.com/repos/org1/repo1",
		},
		"head_commit": map[string]any{"id": headSHA, "message": "tip"},
	}
}

// workflowRunsUpstream is the fake GitHub for the workflow-runs cache tests.
type workflowRunsUpstream struct {
	runsHits  int32
	otherHits int32
	probeHits int32
	runs      func(w http.ResponseWriter, r *http.Request)
	probe     func(w http.ResponseWriter, r *http.Request)
}

func newWorkflowRunsUpstream() *workflowRunsUpstream {
	u := &workflowRunsUpstream{}
	u.runs = func(w http.ResponseWriter, r *http.Request) {
		// total_count deliberately EXCEEDS the page (GitHub's total matching
		// count vs the page length -- exactly what pr-minder's per_page=1
		// probe relies on), and the sha echoes the request's filter so
		// distinct shas produce distinguishable docs.
		sha := strings.ToLower(r.URL.Query().Get("head_sha"))
		servePRJSON(w, map[string]any{
			"total_count": 3,
			"workflow_runs": []any{
				upstreamWorkflowRun(9001, "CI", sha, "completed", "success", "2026-07-01T10:00:30Z"),
				upstreamWorkflowRun(9002, nil, sha, "queued", nil, nil),
			},
		})
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

func (u *workflowRunsUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case r.URL.Path == "/user":
			servePRJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			atomic.AddInt32(&u.runsHits, 1)
			u.runs(w, r)
		case strings.Contains(r.URL.Path, "/actions/runs/"):
			// Deeper run-scoped paths (/actions/runs/{id}/jobs, ...): answer
			// 200 so the passthrough tests can assert the forward happened.
			atomic.AddInt32(&u.otherHits, 1)
			servePRJSON(w, map[string]any{"forwarded": true})
		case len(parts) == 3 && parts[0] == "repos":
			atomic.AddInt32(&u.probeHits, 1)
			u.probe(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
		}
	})
}

func workflowRunsStack(t *testing.T) (http.Handler, *ghdata.Store, *sql.DB, *workflowRunsUpstream) {
	t.Helper()
	u := newWorkflowRunsUpstream()
	router, store, db, _ := newTestStackWithGitHub(t, testAuth(), u.handler())
	return router, store, db, u
}

// TestCachedWorkflowRuns_MissAbsorbHit covers the core flow: the first read
// fetches + absorbs the whole trimmed page (miss), the second serves the
// byte-identical stored doc (hit, zero upstream calls). total_count is copied
// VERBATIM -- GitHub's total matching count, not the page length (here 3 over
// a 2-run page: pr-minder's per_page=1 probe reads exactly this) -- the
// nullable name/conclusion/run_started_at keys survive as null, html_url is
// the pinned consumer-read exception, and the actor/repository/head_commit
// objects plus every other URL field are dropped.
func TestCachedWorkflowRuns_MissAbsorbHit(t *testing.T) {
	router, _, _, u := workflowRunsStack(t)
	target := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runsHits))
	assertNoURLKeys(t, w1.Body.Bytes(), "html_url")
	assert.JSONEq(t, fmt.Sprintf(`{
		"total_count": 3,
		"workflow_runs": [
			{"id": 9001, "name": "CI", "head_sha": %q, "status": "completed",
			 "conclusion": "success", "html_url": "https://github.com/org1/repo1/actions/runs/9001",
			 "created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:05:00Z",
			 "run_started_at": "2026-07-01T10:00:30Z"},
			{"id": 9002, "name": null, "head_sha": %q, "status": "queued",
			 "conclusion": null, "html_url": "https://github.com/org1/repo1/actions/runs/9002",
			 "created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:05:00Z",
			 "run_started_at": null}
		]
	}`, shaTip, shaTip), w1.Body.String())
	assert.NotContains(t, w1.Body.String(), "octocat", "the actor object must be dropped")
	assert.NotContains(t, w1.Body.String(), "full_name", "the repository object must be dropped")
	assert.NotContains(t, w1.Body.String(), "jobs_url", "run-scoped API URLs must be dropped")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runsHits), "hit must not call upstream")
	assertNoURLKeys(t, w2.Body.Bytes(), "html_url")
}

// TestCachedWorkflowRuns_KeyedPerShaAndPage: the sha and the pagination shape
// are both part of the key -- the consumers' two shapes (pr-minder's
// per_page=1 and required-builds' per_page=100&page=N) are independent
// snapshots of one sha, another sha is another row, and a differently-CASED
// sha spelling folds onto the same row (the key is lowercase-normalized).
func TestCachedWorkflowRuns_KeyedPerShaAndPage(t *testing.T) {
	router, _, _, u := workflowRunsStack(t)

	targets := []string{
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&per_page=1",
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&per_page=100",
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&per_page=100&page=2",
		"/repos/org1/repo1/actions/runs?head_sha=" + shaMid + "&per_page=1",
	}
	for _, target := range targets {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		require.Equal(t, "miss", w.Header().Get(cacheHeader), "each (sha, page shape) is its own key: %s", target)
	}
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.runsHits))

	for _, target := range targets {
		w := do(t, router, authedReq("GET", target, nil))
		assert.Equal(t, "hit", w.Header().Get(cacheHeader), target)
	}
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.runsHits), "hits must not call upstream")

	// An uppercased spelling of a cached sha folds onto the same row.
	w := do(t, router, authedReq("GET",
		"/repos/org1/repo1/actions/runs?head_sha="+strings.ToUpper(shaTip)+"&per_page=1", nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "sha casings must fold onto one cache row")
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.runsHits))
}

// TestCachedWorkflowRuns_ShapePassthroughs: shapes the cache does not model
// pass through verbatim, uncached -- crucially the UNFILTERED listing (no
// head_sha: it churns with every run in the repo and is deliberately
// unmodeled), any other filter param, a malformed/short sha, out-of-range
// paging, a non-default Accept, and the deeper /actions/runs/{id}/... paths
// the exact-literal registration never sees.
func TestCachedWorkflowRuns_ShapePassthroughs(t *testing.T) {
	router, _, db, u := workflowRunsStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/actions/runs",                                            // no head_sha: unfiltered
		"/repos/org1/repo1/actions/runs?per_page=1",                                 // still no head_sha
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&branch=main",        // unknown filter param
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&event=push",         // unknown filter param
		"/repos/org1/repo1/actions/runs?head_sha=abc123",                            // short sha
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&per_page=101",       // out of range
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&page=11",            // beyond the modeled cap
		"/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&head_sha=" + shaMid, // repeated
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.runsHits), target)
	}

	// A non-default Accept passes through.
	req := authedReq("GET", "/repos/org1/repo1/actions/runs?head_sha="+shaTip, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w := do(t, router, req)
	assert.Empty(t, w.Header().Get(cacheHeader), "non-default Accept must pass through")

	// Deeper run-scoped paths never reach the route.
	w = do(t, router, authedReq("GET", "/repos/org1/repo1/actions/runs/9001/jobs", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader), "run-scoped paths must pass through")
	assert.Contains(t, w.Body.String(), "forwarded")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.otherHits))

	// Passthroughs stored nothing.
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM workflow_runs_cache`).Scan(&count))
	assert.Zero(t, count, "passthrough shapes must store no row")
}

// TestCachedWorkflowRuns_EmptyListingCacheable: total_count 0 with an empty
// workflow_runs array -- the "no runs yet" verdict pr-minder's zombie probe
// is after -- is a valid, cacheable answer.
func TestCachedWorkflowRuns_EmptyListingCacheable(t *testing.T) {
	router, _, _, u := workflowRunsStack(t)
	u.runs = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	}
	target := "/repos/org1/repo1/actions/runs?head_sha=" + shaTip + "&per_page=1"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"total_count": 0, "workflow_runs": []}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "the zero-runs verdict is a valid cacheable answer")
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.runsHits))
}

// TestCachedWorkflowRuns_WebhookFlush: a CI/job event flushes exactly the
// payload-named sha's pages -- including a QUEUED workflow_job, whose
// delivery the dispatcher drops as ignored but whose invalidation runs first
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

	// A queued workflow_job for shaTip: dropped as ignored by the dispatcher,
	// but the sha's runs pages flush regardless.
	seed(t)
	postWebhook(t, router, "workflow_job", fmt.Sprintf(`{"action":"queued",
		"workflow_job":{"id":1,"run_id":9001,"name":"build","status":"queued","head_sha":%q},
		"repository":{"name":"repo1","owner":{"login":"org1"}}}`, shaTip))
	w := do(t, router, authedReq("GET", tipTarget, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a queued workflow_job must flush its sha's runs pages")
	w = do(t, router, authedReq("GET", midTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "another sha's pages must survive the per-sha flush")

	// A check_run event names its head sha the same way.
	seed(t)
	postWebhook(t, router, "check_run", fmt.Sprintf(`{"action":"created",
		"check_run":{"head_sha":%q,"status":"queued","name":"build",
			"check_suite":{"head_branch":"main"}},
		"repository":{"name":"repo1","owner":{"login":"org1"}}}`, shaTip))
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

// TestCachedWorkflowRuns_Non200NotStored: anything but a well-formed 200 --
// and a 200 whose body lacks total_count or the workflow_runs array -- is
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

	// A 200 missing the absorb-gated fields is replayed unstored too.
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
