package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Commit-CI route tests for the raw statuses LIST (both path spellings) and
// for everything that makes a stored answer stop being served: webhook
// flushes, the TTL backstop, non-200s, and a reveal denial.

func TestCachedStatusesList_MissAbsorbHit(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	alias := "/repos/org1/repo1/statuses/main"

	w1 := do(t, router, authedReq("GET", alias, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusesHits))
	assertNoURLKeys(t, w1.Body.Bytes(), "target_url")
	assert.JSONEq(t, mustJSONString([]any{
		map[string]any{
			"context": "ci/build", "state": "success", "description": "2/2 builds passed",
			"target_url": rbmTargetURL(shaTip),
			"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:05:00Z",
		},
		map[string]any{
			"context": "ci/build", "state": "pending", "description": nil, "target_url": nil,
			"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:05:00Z",
		},
	}), w1.Body.String(), "order preserved newest-first; null keys always emitted")
	assert.NotContains(t, w1.Body.String(), "octocat", "the creator user object must be dropped")
	assert.NotContains(t, w1.Body.String(), "node_id", "per-status ids must be dropped")

	w2 := do(t, router, authedReq("GET", alias, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusesHits), "hit must not call upstream")

	// The modern spelling of the same resource shares the row space.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits/main/statuses", nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader), "both path spellings share one row space")
	assert.Equal(t, w1.Body.String(), w3.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusesHits))

	// A status event flushes the statuses-list snapshot like the other kinds
	// (one commit_ci_cache table, one flush matrix).
	postWebhookJSON(t, router, "status", statusDelivery(shaTip))
	w4 := do(t, router, authedReq("GET", alias, nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader), "a status event must flush the statuses-list snapshot")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.statusesHits))

	// An empty array (a ref with no statuses / a page past the end) is a
	// valid cacheable answer, and the paginated form keys its own row.
	u.statuses = func(w http.ResponseWriter, r *http.Request) { servePRJSON(w, []any{}) }
	paged := alias + "?per_page=100&page=2"
	w5 := do(t, router, authedReq("GET", paged, nil))
	require.Equal(t, http.StatusOK, w5.Code)
	assert.Equal(t, "miss", w5.Header().Get(cacheHeader))
	assert.Equal(t, "[]", w5.Body.String())
	w6 := do(t, router, authedReq("GET", paged, nil))
	assert.Equal(t, "hit", w6.Header().Get(cacheHeader), "an empty page is a valid cacheable answer")

	// An empty alias tail (/statuses/) is not a resource -- passthrough.
	w7 := do(t, router, authedReq("GET", "/repos/org1/repo1/statuses/", nil))
	assert.Empty(t, w7.Header().Get(cacheHeader), "an empty ref tail must pass through")
}

// TestStatusPublishPassthrough is the HIGHEST-BLAST-RADIUS regression guard
// for the statuses-list registration: POST /repos/{o}/{r}/statuses/{sha} is
// required-builds' status PUBLISH -- the org-wide all-builds gate rides on it
// -- and it must reach the upstream proxy untouched (the GET-only route falls
// to chi's MethodNotAllowed -> the passthrough proxy) and be recorded as a
// write, never swallowed by the cache.
func TestStatusPublishPassthrough(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	body := mustJSONString(map[string]any{
		"state": "success", "context": "all-builds", "description": "2/2 builds passed",
		"target_url": rbmTargetURL(shaTip),
	})

	req := authedReq("POST", "/repos/org1/repo1/statuses/"+shaTip, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := do(t, router, req)

	require.Equal(t, http.StatusCreated, w.Code, "the upstream's own 201 must pass back")
	assert.Empty(t, w.Header().Get(cacheHeader), "a status publish is never a cached response")
	assert.Contains(t, w.Body.String(), `"id": 999`, "the upstream's verbatim body must pass back")
	assert.Equal(t, "/repos/org1/repo1/statuses/"+shaTip, u.lastPostPath, "the POST must reach upstream on its exact path")
	assert.JSONEq(t, body, u.lastPostBody, "the POST body must reach upstream untouched")
}

// TestCachedCommitCI_WebhookFlush walks what each delivery does to a ref's
// three snapshot kinds. Two rules decide every row of the table:
//
//   - The surfaces are DISJOINT. A commit status never appears in a check-runs
//     listing and a check run never appears in a commit's statuses, so a CI
//     delivery that dropped all three was re-fetching answers it could not
//     have changed.
//   - A `status` delivery CARRIES the status, so the documents it can identify
//     are rewritten and keep serving. The combined status names the sha it
//     resolved to, which is what makes a branch-form row usable; the raw list
//     is a bare array with no sha in it, so only the sha-form row is provably
//     about this commit and the branch spelling still flushes.
//
// (Round 2 made the CI-event grain per-ref: the payloads below all name "main"
// -- via the status branches array or the suite head_branch -- exactly like
// GitHub's real deliveries; a per-branch survival case lives in the dispatcher
// tests.)
func TestCachedCommitCI_WebhookFlush(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	statusTarget := "/repos/org1/repo1/commits/main/status"
	checksTarget := "/repos/org1/repo1/commits/main/check-runs"
	listTarget := "/repos/org1/repo1/commits/main/statuses"
	targets := [3]string{statusTarget, checksTarget, listTarget}

	seed := func(t *testing.T) {
		t.Helper()
		for _, target := range targets {
			do(t, router, authedReq("GET", target, nil))
			w := do(t, router, authedReq("GET", target, nil))
			require.Equal(t, "hit", w.Header().Get(cacheHeader), "seed must serve: %s", target)
		}
	}

	const (
		hit  = "hit"
		miss = "miss"
	)
	for _, tc := range []struct {
		event   string
		payload map[string]any
		want    [3]string // status, check-runs, statuses-list
		why     string
	}{
		{"status", statusDelivery(shaTip), [3]string{hit, hit, miss},
			"a status rewrites the combined doc it names, cannot touch check runs, and flushes the branch-form list"},
		{"check_run", map[string]any{
			"action": "completed",
			"check_run": map[string]any{
				"head_sha": shaTip, "status": "completed", "conclusion": "success", "name": "build",
				"check_suite": map[string]any{"head_branch": "main"},
			},
			"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
		}, [3]string{hit, miss, hit}, "a check run moves only the check-runs listing"},
		{"check_suite", map[string]any{
			"action": "completed",
			"check_suite": map[string]any{
				"head_sha": shaTip, "head_branch": "main", "status": "completed", "conclusion": "success",
			},
			"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
		}, [3]string{hit, miss, hit}, "a check suite moves only the check-runs listing"},
		{"push", map[string]any{"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}}},
			[3]string{miss, miss, miss}, "a push with no usable ref keeps the repo-wide fallback"},
		{"repository", map[string]any{"action": "privatized", "repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}}},
			[3]string{miss, miss, miss}, "a repository event makes every row wrong, not stale"},
	} {
		seed(t)
		before := [3]int32{
			atomic.LoadInt32(&u.statusHits), atomic.LoadInt32(&u.checkRunsHits), atomic.LoadInt32(&u.statusesHits),
		}
		postWebhookJSON(t, router, tc.event, tc.payload)

		for i, target := range targets {
			w := do(t, router, authedReq("GET", target, nil))
			assert.Equal(t, tc.want[i], w.Header().Get(cacheHeader),
				"%s -> %s: %s", tc.event, target, tc.why)
		}
		upstream := [3]int32{
			atomic.LoadInt32(&u.statusHits), atomic.LoadInt32(&u.checkRunsHits), atomic.LoadInt32(&u.statusesHits),
		}
		for i := range targets {
			extra := int32(0)
			if tc.want[i] == miss {
				extra = 1
			}
			assert.Equal(t, before[i]+extra, upstream[i],
				"%s -> %s: a served answer must cost no upstream call", tc.event, targets[i])
		}
	}
}

// A check run is UPDATED IN PLACE upstream -- the same id moves queued ->
// in_progress -> completed -- so a delivery states the new value of one entry
// and the stored page keeps its shape. The pin is the same as the status one:
// the rewritten page must be what a fetch would now return, byte for byte.
func TestCachedCommitCI_CheckRunEventRewritesThePage(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	target := "/repos/org1/repo1/commits/" + shaTip + "/check-runs"

	runs := func(status string, conclusion, completedAt any) map[string]any {
		return map[string]any{"total_count": 2, "check_runs": []any{
			upstreamCheckRun(101, "build/"+shaTip, "completed", "success", "2026-07-01T10:00:00Z", "2026-07-01T10:04:00Z"),
			upstreamCheckRun(102, "test/"+shaTip, status, conclusion, "2026-07-01T10:01:00Z", completedAt),
		}}
	}
	u.checkRuns = func(w http.ResponseWriter, _ *http.Request) {
		servePRJSON(w, runs("in_progress", nil, nil))
	}
	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunsHits))

	finished := upstreamCheckRun(102, "test/"+shaTip, "completed", "success",
		"2026-07-01T10:01:00Z", "2026-07-01T10:06:00Z")
	postWebhookJSON(t, router, "check_run", map[string]any{
		"action": "completed", "check_run": finished, "repository": fixtureRepo(),
	})

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "the delivery answered it; nothing to re-fetch")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunsHits), "a rewrite must cost no upstream call")

	u.checkRuns = func(w http.ResponseWriter, _ *http.Request) {
		servePRJSON(w, runs("completed", "success", "2026-07-01T10:06:00Z"))
	}
	fresh := do(t, router, authedReq("GET", target+"?per_page=100", nil))
	require.Equal(t, "miss", fresh.Header().Get(cacheHeader), "a different page shape is its own row")
	assert.Equal(t, fresh.Body.String(), w.Body.String(),
		"the rewritten page must be byte-identical to the fetch it saved")
}

// TestCachedCommitCI_StatusEventRewritesTheCombinedDoc is the identity pin for
// the rewrite: a status delivery's effect on a stored document must be
// indistinguishable from the fetch it saved. The stored answer after the
// delivery is byte-compared against what the same route returns once upstream
// reports the new status -- so a drifted field order, a wrong rollup, or an
// entry in the wrong position fails here rather than in a consumer.
func TestCachedCommitCI_StatusEventRewritesTheCombinedDoc(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	target := "/repos/org1/repo1/commits/" + shaTip + "/status"

	// The commit starts with lint pending, so the rollup is pending.
	u.status = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, upstreamCombinedStatus(shaTip, "pending", []any{
			upstreamStatusItem("ci/build", "success", "2/2 builds passed", rbmTargetURL(shaTip)),
			upstreamStatusItem("lint", "pending", nil, ""),
		}))
	}
	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.statusHits))

	// lint finishes. The delivery states the whole status, so the stored
	// document is rewritten rather than dropped.
	delivery := statusDelivery(shaTip)
	delivery["context"] = "lint"
	delivery["state"] = "success"
	delivery["description"] = nil
	delivery["target_url"] = nil
	postWebhookJSON(t, router, "status", delivery)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "the delivery answered it; nothing to re-fetch")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusHits), "a rewrite must cost no upstream call")

	// What GitHub would now answer: lint's entry moved to the END (the array
	// is oldest-first and a re-posted context is a NEW status object) and the
	// rollup went to success.
	u.status = func(w http.ResponseWriter, r *http.Request) {
		servePRJSON(w, upstreamCombinedStatus(shaTip, "success", []any{
			upstreamStatusItem("ci/build", "success", "2/2 builds passed", rbmTargetURL(shaTip)),
			map[string]any{
				"context": "lint", "state": "success", "description": nil, "target_url": nil,
				"created_at": "2026-07-01T11:00:00Z", "updated_at": "2026-07-01T11:00:00Z",
				"id": 1, "node_id": "SC_x", "url": "https://api.github.com/x", "avatar_url": "https://a",
			},
		}))
	}
	fresh := do(t, router, authedReq("GET", target+"?per_page=100", nil))
	require.Equal(t, "miss", fresh.Header().Get(cacheHeader), "a different page shape is its own row")
	assert.Equal(t, fresh.Body.String(), w.Body.String(),
		"the rewritten document must be byte-identical to the fetch it saved")
}

// TestCachedCommitCI_TTLBackstopExpiry: even with webhooks silent, a snapshot
// expires after its TTL -- a missed CI/push delivery can't serve stale CI
// state forever.
func TestCachedCommitCI_TTLBackstopExpiry(t *testing.T) {
	router, _, db, u := commitCIStack(t)
	target := "/repos/org1/repo1/commits/main/status"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.statusHits))

	_, err := db.Exec(`UPDATE commit_ci_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired snapshot is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.statusHits))
}

// TestCachedCommitCI_Non200NotStored: 404 (unknown ref -- it can be pushed
// later), 5xx -- anything but a 200 -- is relayed verbatim and stores nothing,
// on both routes.
func TestCachedCommitCI_Non200NotStored(t *testing.T) {
	router, _, db, u := commitCIStack(t)
	notFound := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
	}
	u.status, u.checkRuns = notFound, notFound

	for _, target := range []string{
		"/repos/org1/repo1/commits/ghostbranch/status",
		"/repos/org1/repo1/commits/ghostbranch/check-runs",
	} {
		for i := 1; i <= 2; i++ {
			w := do(t, router, authedReq("GET", target, nil))
			require.Equal(t, http.StatusNotFound, w.Code, target)
			assert.Empty(t, w.Header().Get(cacheHeader), "a non-200 must be replayed unstored: %s", target)
		}
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.statusHits))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.checkRunsHits))

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM commit_ci_cache`).Scan(&count))
	assert.Zero(t, count, "a non-200 answer must store no snapshot")
}

// TestCachedCommitCI_RevealDenied: an unauthorized caller gets GitHub's own
// relayed denial and never reaches the CI fetch; the repeat request is
// answered from the deny cache without touching GitHub.
func TestCachedCommitCI_RevealDenied(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	}

	for _, target := range []string{
		"/repos/org1/ghost/commits/main/status",
		"/repos/org1/ghost/commits/main/check-runs",
	} {
		w1 := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusNotFound, w1.Code, target)
		assert.Equal(t, "miss", w1.Header().Get(cacheHeader), "a fresh probe denial is a miss: %s", target)
		assertNoURLKeys(t, w1.Body.Bytes())

		w2 := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusNotFound, w2.Code, target)
		assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a cached deny verdict answers without GitHub: %s", target)
	}
	// One probe per denied resource kind+key; the CI endpoints were never hit.
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.statusHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.checkRunsHits))
}

// rbmTargetURL is the required-builds-manager link the fake statuses upstream
// attaches to a status. It is built here so the URL is placed into the JSON by
// %q rather than pasted between the literal's own quotes.
func rbmTargetURL(sha string) string {
	return "https://rbm.example.com/b/org1/repo1/" + sha
}

// statusDelivery is GitHub's `status` payload for a build on main: the
// branches array is what makes the flush per-ref rather than repo-wide.
func statusDelivery(sha string) map[string]any {
	return map[string]any{
		"sha": sha, "state": "success", "context": "ci/build",
		"description": "2/2 builds passed",
		"target_url":  rbmTargetURL(sha),
		// A status delivery always carries both timestamps, and they are not
		// decoration: created_at is what places the entry in a stored
		// document's ordering, so a fixture without them exercises only the
		// fallback flush.
		"created_at": "2026-07-01T11:00:00Z",
		"updated_at": "2026-07-01T11:00:00Z",
		"branches":   []any{map[string]any{"name": "main"}},
		"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
	}
}
