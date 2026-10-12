package api

import (
	"net/http"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Single workflow-run read tests.

const (
	singleRunTarget     = "/repos/org1/repo1/actions/runs/4242"
	singleRunAllowedURL = "html_url"
	singleRunJobsURL    = "jobs_url"
)

var singleRunPath = regexp.MustCompile(`^/repos/[^/]+/[^/]+/actions/runs/[0-9]+$`)

func singleRunObject(status string, conclusion any, updatedAt string) map[string]any {
	user := map[string]any{"login": "alice", "id": 7, "type": "User", "url": "https://api.github.com/users/alice", "avatar_url": "https://a"}
	repo := map[string]any{
		"id": 99, "name": "repo1", "full_name": "org1/repo1", "private": true, "owner": user,
		"html_url": "https://github.com/org1/repo1", "hooks_url": "https://api.github.com/x",
	}
	return map[string]any{
		"id": 4242, "name": "CI", "node_id": "WFR_kw", "head_branch": "main", "head_sha": shaTip,
		"path": ".github/workflows/ci.yml", "display_title": "fix things", "run_number": 12,
		"event": "push", "status": status, "conclusion": conclusion, "workflow_id": 161335,
		"check_suite_id": 555, "check_suite_node_id": "CS_kw",
		"url":      "https://api.github.com/repos/org1/repo1/actions/runs/4242",
		"html_url": "https://github.com/org1/repo1/actions/runs/4242",
		"pull_requests": []any{map[string]any{
			"id": 1, "number": 17, "url": "https://api.github.com/pulls/17",
			"head": map[string]any{"ref": "feature", "sha": shaTip, "repo": map[string]any{"id": 99, "name": "repo1", "url": "https://api.github.com/r"}},
			"base": map[string]any{"ref": "main", "sha": shaBase, "repo": map[string]any{"id": 99, "name": "repo1", "url": "https://api.github.com/r"}},
		}},
		"created_at": "2026-07-01T10:00:00Z", "updated_at": updatedAt,
		"actor": user, "triggering_actor": user, "run_attempt": 1,
		"referenced_workflows": []any{}, "run_started_at": "2026-07-01T10:00:05Z",
		"jobs_url":      "https://api.github.com/repos/org1/repo1/actions/runs/4242/jobs",
		"logs_url":      "https://api.github.com/repos/org1/repo1/actions/runs/4242/logs",
		"artifacts_url": "https://api.github.com/x", "cancel_url": "https://api.github.com/x",
		"rerun_url": "https://api.github.com/x", "workflow_url": "https://api.github.com/x",
		"previous_attempt_url": nil,
		"head_commit": map[string]any{
			"id": shaTip, "tree_id": shaTree1, "message": "fix things", "timestamp": "2026-07-01T09:59:00Z",
			"author": map[string]any{"name": "Alice", "email": "a@example.com"}, "committer": map[string]any{"name": "Alice", "email": "a@example.com"},
		},
		"repository": repo, "head_repository": repo,
	}
}

// singleRunStack answers the run read from *run and counts its calls.
func singleRunStack(t *testing.T, run *map[string]any) (http.Handler, *ghdata.Store, *int32, *respCacheUpstream) {
	t.Helper()
	router, store, _, u := respCacheStack(t)
	var calls int32
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !singleRunPath.MatchString(r.URL.Path) {
			return false
		}
		atomic.AddInt32(&calls, 1)
		if r.URL.RawQuery != "" {
			// Only a passthrough forwards a query; the cached fetch never does.
			w.Header().Set("X-Test-Forwarded-Query", r.URL.RawQuery)
		}
		answer := *run
		if r.URL.Query().Get("exclude_pull_requests") == "true" {
			// GitHub's own behavior, so a fetch that kept the parameter would lose the pull requests.
			answer = map[string]any{}
			for k, v := range *run {
				answer[k] = v
			}
			answer["pull_requests"] = []any{}
		}
		writeGitHubJSON(w, answer)
		return true
	}
	return router, store, &calls, u
}

func TestCachedWorkflowRun_MissAbsorbHit(t *testing.T) {
	run := singleRunObject("in_progress", nil, "2026-07-01T10:01:00Z")
	router, _, calls, _ := singleRunStack(t, &run)

	w1 := do(t, router, authedReq("GET", singleRunTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes(), singleRunAllowedURL, singleRunJobsURL)
	assert.JSONEq(t, mustJSONString(map[string]any{
		"id": 4242, "name": "CI", "node_id": "WFR_kw", "head_branch": "main", "head_sha": shaTip,
		"path": ".github/workflows/ci.yml", "display_title": "fix things", "run_number": 12,
		"event": "push", "status": "in_progress", "conclusion": nil, "workflow_id": 161335,
		"check_suite_id": 555, "html_url": "https://github.com/org1/repo1/actions/runs/4242",
		"pull_requests": []any{map[string]any{
			"id": 1, "number": 17,
			"head": map[string]any{"ref": "feature", "sha": shaTip, "repo": map[string]any{"id": 99, "name": "repo1"}},
			"base": map[string]any{"ref": "main", "sha": shaBase, "repo": map[string]any{"id": 99, "name": "repo1"}},
		}},
		"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:01:00Z",
		"actor":       map[string]any{"login": "alice", "id": 7, "type": "User"},
		"run_attempt": 1, "run_started_at": "2026-07-01T10:00:05Z",
		"triggering_actor": map[string]any{"login": "alice", "id": 7, "type": "User"},
		"jobs_url":         "https://api.github.com/repos/org1/repo1/actions/runs/4242/jobs",
		"head_commit": map[string]any{
			"id": shaTip, "tree_id": shaTree1, "message": "fix things", "timestamp": "2026-07-01T09:59:00Z",
			"author": map[string]any{"name": "Alice", "email": "a@example.com"}, "committer": map[string]any{"name": "Alice", "email": "a@example.com"},
		},
		"repository":      map[string]any{"id": 99, "name": "repo1", "full_name": "org1/repo1", "private": true, "owner": map[string]any{"login": "alice", "id": 7, "type": "User"}},
		"head_repository": map[string]any{"id": 99, "name": "repo1", "full_name": "org1/repo1", "private": true, "owner": map[string]any{"login": "alice", "id": 7, "type": "User"}},
	}), w1.Body.String())

	req := authedReq("GET", singleRunTarget, nil)
	req.Header.Set("Accept", ghCLIAccept)
	w2 := do(t, router, req)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// exclude_pull_requests only empties pull_requests; one stored object answers
// both spellings.
func TestCachedWorkflowRun_ExcludePullRequests(t *testing.T) {
	run := singleRunObject("completed", "success", "2026-07-01T10:05:00Z")
	router, _, calls, _ := singleRunStack(t, &run)

	w1 := do(t, router, authedReq("GET", singleRunTarget+"?exclude_pull_requests=true", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Contains(t, w1.Body.String(), `"pull_requests":[]`)

	w2 := do(t, router, authedReq("GET", singleRunTarget+"?exclude_pull_requests=false", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Contains(t, w2.Body.String(), `"number":17`)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// A workflow_run delivery carries the whole run, and the read serves what
// it applied without a fetch.
func TestCachedWorkflowRun_DeliveryApplies(t *testing.T) {
	run := singleRunObject("in_progress", nil, "2026-07-01T10:01:00Z")
	router, _, calls, _ := singleRunStack(t, &run)
	require.Equal(t, "miss", do(t, router, authedReq("GET", singleRunTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "workflow_run", map[string]any{
		"action":       "completed",
		"workflow_run": singleRunObject("completed", "failure", "2026-07-01T10:09:00Z"),
		"repository":   fixtureRepo(),
	})

	w := do(t, router, authedReq("GET", singleRunTarget, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Contains(t, w.Body.String(), `"status":"completed"`)
	assert.Contains(t, w.Body.String(), `"conclusion":"failure"`)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls), "the delivery answered it")
}

// A run the mirror only heard about by delivery is served without any fetch.
func TestCachedWorkflowRun_DeliveryAloneServes(t *testing.T) {
	run := singleRunObject("queued", nil, "2026-07-01T10:00:00Z")
	router, _, calls, _ := singleRunStack(t, &run)
	postWebhookJSON(t, router, "workflow_run", map[string]any{
		"action": "requested", "workflow_run": run, "repository": fixtureRepo(),
	})

	w := do(t, router, authedReq("GET", singleRunTarget, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Equal(t, int32(0), atomic.LoadInt32(calls))
}

// A job delivery names its run but not the whole object, so the read still
// fetches.
func TestCachedWorkflowRun_JobDeliveryIsNotEnough(t *testing.T) {
	run := singleRunObject("in_progress", nil, "2026-07-01T10:01:00Z")
	router, _, calls, _ := singleRunStack(t, &run)
	postWebhookJSON(t, router, "workflow_job", map[string]any{
		"action": "in_progress",
		"workflow_job": map[string]any{
			"id": 1, "run_id": 4242, "run_attempt": 1, "name": "build", "workflow_name": "CI",
			"status": "in_progress", "head_sha": shaTip, "head_branch": "main",
		},
		"repository": fixtureRepo(),
	})

	assert.Equal(t, "miss", do(t, router, authedReq("GET", singleRunTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestCachedWorkflowRun_RefusalIsAVerdict(t *testing.T) {
	run := singleRunObject("queued", nil, "2026-07-01T10:00:00Z")
	router, _, calls, u := singleRunStack(t, &run)
	status := http.StatusForbidden
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !singleRunPath.MatchString(r.URL.Path) {
			return false
		}
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration","status":"403"}`))
		return true
	}

	w1 := do(t, router, authedReq("GET", singleRunTarget, nil))
	require.Equal(t, http.StatusForbidden, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	w2 := do(t, router, authedReq("GET", singleRunTarget, nil))
	require.Equal(t, http.StatusForbidden, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"message":"Resource not accessible by integration","status":"403"}`, w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	status = http.StatusBadGateway
	other := "/repos/org1/repo1/actions/runs/4243"
	for i := int32(2); i <= 3; i++ {
		w := do(t, router, authedReq("GET", other, nil))
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, i, atomic.LoadInt32(calls))
	}
}

func TestCachedWorkflowRun_ShapeGuardsPassThrough(t *testing.T) {
	run := singleRunObject("queued", nil, "2026-07-01T10:00:00Z")
	router, _, calls, _ := singleRunStack(t, &run)
	u := func(target string) *http.Request { return authedReq("GET", target, nil) }

	for _, req := range []*http.Request{
		u(singleRunTarget + "?per_page=1"),
		u(singleRunTarget + "?exclude_pull_requests=yes"),
		u(singleRunTarget + "?exclude_pull_requests=true&exclude_pull_requests=false"),
	} {
		w := do(t, router, req)
		assert.Empty(t, w.Header().Get(cacheHeader), req.URL.String())
	}
	req := u(singleRunTarget)
	req.Header.Set("Accept", "application/vnd.github.diff")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Empty(t, do(t, router, u("/repos/org1/repo1/actions/runs/abc")).Header().Get(cacheHeader))
	assert.Equal(t, int32(4), atomic.LoadInt32(calls), "every numeric-id passthrough reaches GitHub")
}

func TestWorkflowRunServable_Bounds(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	cases := []struct {
		row  ghdata.WorkflowRun
		want bool
	}{
		{ghdata.WorkflowRun{Status: "completed", Detail: "{}", DetailAt: at(23 * time.Hour)}, true},
		{ghdata.WorkflowRun{Status: "completed", Detail: "{}", DetailAt: at(25 * time.Hour)}, false},
		{ghdata.WorkflowRun{Status: "in_progress", Detail: "{}", DetailAt: at(30 * time.Second)}, true},
		{ghdata.WorkflowRun{Status: "in_progress", Detail: "{}", DetailAt: at(2 * time.Minute)}, false},
		{ghdata.WorkflowRun{Status: "completed", DetailAt: at(time.Second)}, false},
	}
	for i, c := range cases {
		assert.Equal(t, c.want, ghdata.WorkflowRunServable(c.row, now), "case %d", i)
	}
}
