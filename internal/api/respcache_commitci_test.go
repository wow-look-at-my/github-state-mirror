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

// upstreamCombinedStatus builds a GitHub-shaped combined-status response: URL
// clutter at every level (url/commit_url, the full repository object, per-
// status avatar/target URLs) around the fields the model absorbs. The ref is
// embedded in each status context so distinct refs produce distinguishable
// trimmed docs.
func upstreamCombinedStatus(ref, state string, statuses []any) map[string]any {
	return map[string]any{
		"state":       state,
		"sha":         shaTip,
		"total_count": len(statuses),
		"statuses":    statuses,
		"commit_url":  "https://api.github.com/repos/org1/repo1/commits/" + shaTip,
		"url":         "https://api.github.com/repos/org1/repo1/commits/" + ref + "/status",
		"repository": map[string]any{
			"id": 1, "name": "repo1", "full_name": "org1/repo1", "private": true,
			"html_url": "https://github.com/org1/repo1",
			"owner":    map[string]any{"login": "org1", "avatar_url": "https://a", "url": "https://api.github.com/users/org1"},
		},
	}
}

// upstreamStatusItem builds one GitHub-shaped statuses entry, including the
// avatar_url/target_url/url fields the rebuild must drop. description may be
// nil (GitHub sends "description": null when unset).
func upstreamStatusItem(context, state string, description any, targetURL string) map[string]any {
	return map[string]any{
		"url":         "https://api.github.com/repos/org1/repo1/statuses/" + shaTip,
		"avatar_url":  "https://avatars.githubusercontent.com/u/1",
		"id":          123456,
		"node_id":     "SC_xyz",
		"state":       state,
		"description": description,
		"target_url":  targetURL,
		"context":     context,
		"created_at":  "2026-07-01T10:00:00Z",
		"updated_at":  "2026-07-01T10:05:00Z",
	}
}

// upstreamCheckRun builds one GitHub-shaped check_runs entry, including the
// url/html_url/details_url, the output object (with annotations_url), the
// check_suite, and the full app object the rebuild trims to {id}. conclusion,
// startedAt, and completedAt may be nil (a queued/in-progress run).
func upstreamCheckRun(id int, name, status string, conclusion, startedAt, completedAt any) map[string]any {
	return map[string]any{
		"id":           id,
		"head_sha":     shaTip,
		"node_id":      "CR_xyz",
		"external_id":  "42",
		"url":          fmt.Sprintf("https://api.github.com/repos/org1/repo1/check-runs/%d", id),
		"html_url":     fmt.Sprintf("https://github.com/org1/repo1/runs/%d", id),
		"details_url":  "https://ci.example.com/builds/1",
		"status":       status,
		"conclusion":   conclusion,
		"started_at":   startedAt,
		"completed_at": completedAt,
		"name":         name,
		"output": map[string]any{
			"title": "Build report", "summary": "never stored", "text": nil,
			"annotations_count": 0,
			"annotations_url":   fmt.Sprintf("https://api.github.com/repos/org1/repo1/check-runs/%d/annotations", id),
		},
		"check_suite": map[string]any{"id": 5},
		"app": map[string]any{
			"id": 777, "slug": "test-ci", "name": "Test CI",
			"html_url":     "https://github.com/apps/test-ci",
			"external_url": "https://ci.example.com",
			"owner":        map[string]any{"login": "org1", "avatar_url": "https://a"},
		},
		"pull_requests": []any{},
	}
}

// commitCIUpstream is the fake GitHub for the commit-CI cache tests.
type commitCIUpstream struct {
	statusHits    int32
	checkRunsHits int32
	otherHits     int32
	probeHits     int32
	status        func(w http.ResponseWriter, r *http.Request)
	checkRuns     func(w http.ResponseWriter, r *http.Request)
	probe         func(w http.ResponseWriter, r *http.Request)
}

// refOf extracts the {ref} between /commits/ and the trailing literal.
func refOf(path, suffix string) string {
	tail := strings.SplitN(path, "/commits/", 2)[1]
	return strings.TrimSuffix(tail, suffix)
}

func newCommitCIUpstream() *commitCIUpstream {
	u := &commitCIUpstream{}
	u.status = func(w http.ResponseWriter, r *http.Request) {
		ref := refOf(r.URL.Path, "/status")
		servePRJSON(w, upstreamCombinedStatus(ref, "success", []any{
			upstreamStatusItem("ci/"+ref, "success", "2/2 builds passed", "https://rbm.example.com/b/org1/repo1/"+shaTip),
			upstreamStatusItem("lint", "success", nil, ""),
		}))
	}
	u.checkRuns = func(w http.ResponseWriter, r *http.Request) {
		ref := refOf(r.URL.Path, "/check-runs")
		servePRJSON(w, map[string]any{
			"total_count": 2,
			"check_runs": []any{
				upstreamCheckRun(101, "build/"+ref, "completed", "success", "2026-07-01T10:00:00Z", "2026-07-01T10:04:00Z"),
				upstreamCheckRun(102, "test/"+ref, "in_progress", nil, "2026-07-01T10:01:00Z", nil),
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

func (u *commitCIUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case r.URL.Path == "/user":
			servePRJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
		case strings.Contains(r.URL.Path, "/commits/"):
			// Dispatch by the tail after /commits/ with the same suffix-cut
			// rule the mirror uses, so a branch literally NAMED "status"
			// (tail "status" -- a single-commit read) lands in the
			// "forwarded" bucket, not the status one.
			tail := strings.SplitN(r.URL.Path, "/commits/", 2)[1]
			if ref, ok := strings.CutSuffix(tail, "/status"); ok && ref != "" {
				atomic.AddInt32(&u.statusHits, 1)
				u.status(w, r)
				return
			}
			if ref, ok := strings.CutSuffix(tail, "/check-runs"); ok && ref != "" {
				atomic.AddInt32(&u.checkRunsHits, 1)
				u.checkRuns(w, r)
				return
			}
			// The unmodeled subtree tails (single-commit read, /statuses,
			// /check-suites): answer 200 so the passthrough tests can assert
			// the forward happened.
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

func commitCIStack(t *testing.T) (http.Handler, *ghdata.Store, *sql.DB, *commitCIUpstream) {
	t.Helper()
	u := newCommitCIUpstream()
	router, store, db, _ := newTestStackWithGitHub(t, testAuth(), u.handler())
	return router, store, db, u
}

// TestCachedCommitStatus_MissAbsorbHit covers the combined-status core flow:
// the first read fetches + absorbs (miss), the second serves the identical
// trimmed body from state (hit, zero upstream calls), and the rebuild drops
// every URL field (per-status target_url/avatar_url/url included), the full
// repository object, and the per-status ids -- while a null description
// survives as null.
func TestCachedCommitStatus_MissAbsorbHit(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	target := "/repos/org1/repo1/commits/main/status"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"state": "success",
		"sha": %q,
		"total_count": 2,
		"statuses": [
			{"context": "ci/main", "state": "success", "description": "2/2 builds passed",
			 "created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:05:00Z"},
			{"context": "lint", "state": "success", "description": null,
			 "created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:05:00Z"}
		]
	}`, shaTip), w1.Body.String())
	assert.NotContains(t, w1.Body.String(), "rbm.example.com", "the per-status target_url must be dropped")
	assert.NotContains(t, w1.Body.String(), "full_name", "the repository object must be dropped")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusHits), "hit must not call upstream")
}

// TestCachedCheckRuns_MissAbsorbHit covers the check-runs core flow: miss
// then identical hit, with the rebuild dropping url/html_url/details_url,
// node_id/external_id, check_suite, pull_requests, and the unbounded output
// object -- while the nullable conclusion/completed_at of an in-progress run
// survive as null and the app object is trimmed to {id}.
func TestCachedCheckRuns_MissAbsorbHit(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	target := "/repos/org1/repo1/commits/main/check-runs"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunsHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"total_count": 2,
		"check_runs": [
			{"id": 101, "head_sha": %q, "name": "build/main", "status": "completed",
			 "conclusion": "success", "started_at": "2026-07-01T10:00:00Z",
			 "completed_at": "2026-07-01T10:04:00Z", "app": {"id": 777}},
			{"id": 102, "head_sha": %q, "name": "test/main", "status": "in_progress",
			 "conclusion": null, "started_at": "2026-07-01T10:01:00Z",
			 "completed_at": null, "app": {"id": 777}}
		]
	}`, shaTip, shaTip), w1.Body.String())
	assert.NotContains(t, w1.Body.String(), "never stored", "the output object must be dropped")
	assert.NotContains(t, w1.Body.String(), "check_suite", "the check_suite object must be dropped")
	assert.NotContains(t, w1.Body.String(), "slug", "the app object must be trimmed to its id")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunsHits), "hit must not call upstream")
}

// TestCachedCommitCI_RefKeying: every verbatim ref spelling -- a branch, a
// sha, a slashed branch -- is its own row, slashed refs route into the cached
// routes (the suffix-anchored subtree dispatch), and the status and
// check-runs snapshots for one ref are independent rows.
func TestCachedCommitCI_RefKeying(t *testing.T) {
	router, _, _, u := commitCIStack(t)

	bodies := map[string]string{}
	for _, target := range []string{
		"/repos/org1/repo1/commits/main/status",
		"/repos/org1/repo1/commits/" + shaTip + "/status",
		"/repos/org1/repo1/commits/claude/my-branch/status", // slashed branch
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		require.Equal(t, "miss", w.Header().Get(cacheHeader), "each ref spelling is its own key: %s", target)
		bodies[target] = w.Body.String()
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.statusHits))
	assert.Contains(t, bodies["/repos/org1/repo1/commits/claude/my-branch/status"], "ci/claude/my-branch",
		"the slashed ref must reach upstream intact")

	// The check-runs snapshot for a ref is independent of its status snapshot:
	// a cached status must not answer a check-runs read.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/commits/main/check-runs", nil))
	require.Equal(t, "miss", w.Header().Get(cacheHeader), "status and check-runs are independent snapshots")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.checkRunsHits))

	// Slashed refs hit check-runs too.
	w = do(t, router, authedReq("GET", "/repos/org1/repo1/commits/claude/my-branch/check-runs", nil))
	require.Equal(t, "miss", w.Header().Get(cacheHeader))
	assert.Contains(t, w.Body.String(), "build/claude/my-branch")

	// Each key serves from its own row.
	for target, body := range bodies {
		w := do(t, router, authedReq("GET", target, nil))
		assert.Equal(t, "hit", w.Header().Get(cacheHeader), target)
		assert.Equal(t, body, w.Body.String(), target)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.statusHits), "hits must not call upstream")
}

// TestCachedCommitCI_PassthroughShapes: shapes the cache does not model pass
// through verbatim, uncached, every time -- any query param, a non-default
// Accept, and every OTHER /commits/* subtree tail (the single-commit read,
// the raw /statuses list, /check-suites, a branch literally named "status"
// read as a single commit).
func TestCachedCommitCI_PassthroughShapes(t *testing.T) {
	router, _, _, u := commitCIStack(t)

	// Query params are not modeled on either route.
	for _, target := range []string{
		"/repos/org1/repo1/commits/main/status?per_page=100",
		"/repos/org1/repo1/commits/main/status?page=2",
		"/repos/org1/repo1/commits/main/check-runs?per_page=100&page=2",
		"/repos/org1/repo1/commits/main/check-runs?check_name=build",
		"/repos/org1/repo1/commits/main/check-runs?filter=latest",
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.statusHits))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.checkRunsHits))

	// A non-default Accept passes through on both routes.
	for _, target := range []string{
		"/repos/org1/repo1/commits/main/status",
		"/repos/org1/repo1/commits/main/check-runs",
	} {
		req := authedReq("GET", target, nil)
		req.Header.Set("Accept", "application/vnd.github.raw")
		w := do(t, router, req)
		assert.Empty(t, w.Header().Get(cacheHeader), "non-default Accept must pass through: %s", target)
	}

	// The rest of the /commits/* subtree stays passthrough: the single-commit
	// read (including a branch literally named "status" or "check-runs" --
	// their tails have no /status "/check-runs" SUFFIX), the raw /statuses
	// list, and /check-suites.
	for _, target := range []string{
		"/repos/org1/repo1/commits/" + shaTip,
		"/repos/org1/repo1/commits/status",
		"/repos/org1/repo1/commits/check-runs",
		"/repos/org1/repo1/commits/main/statuses",
		"/repos/org1/repo1/commits/main/check-suites",
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "other subtree tails must pass through: %s", target)
		assert.Contains(t, w.Body.String(), "forwarded", target)
	}
	assert.Equal(t, int32(5), atomic.LoadInt32(&u.otherHits))

	// Passthroughs stored nothing: a cacheable shape still misses.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/commits/main/status", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
}

// TestCachedCommitCI_WebhookFlush: each of status/check_run/check_suite (CI
// state moved on the ref the payload names -- the branch these snapshots are
// keyed by), push with no usable ref (repo-wide fallback), and repository
// events flushes BOTH of the ref's snapshot kinds. (Round 2 made the CI-event
// flush per-ref: the payloads below all name "main" -- via the status
// branches array or the suite head_branch -- exactly like GitHub's real
// deliveries; a per-branch survival case lives in the dispatcher tests.)
func TestCachedCommitCI_WebhookFlush(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	statusTarget := "/repos/org1/repo1/commits/main/status"
	checksTarget := "/repos/org1/repo1/commits/main/check-runs"

	seed := func(t *testing.T) {
		t.Helper()
		for _, target := range []string{statusTarget, checksTarget} {
			do(t, router, authedReq("GET", target, nil))
			w := do(t, router, authedReq("GET", target, nil))
			require.Equal(t, "hit", w.Header().Get(cacheHeader), "seed must serve: %s", target)
		}
	}

	for _, tc := range []struct{ event, body string }{
		{"status", fmt.Sprintf(`{"sha":%q,"state":"success","context":"ci/build",
			"branches":[{"name":"main"}],
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`, shaTip)},
		{"check_run", fmt.Sprintf(`{"action":"completed",
			"check_run":{"head_sha":%q,"status":"completed","conclusion":"success","name":"build",
				"check_suite":{"head_branch":"main"}},
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`, shaTip)},
		{"check_suite", fmt.Sprintf(`{"action":"completed",
			"check_suite":{"head_sha":%q,"head_branch":"main","status":"completed","conclusion":"success"},
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`, shaTip)},
		{"push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
		{"repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"}}}`},
	} {
		seed(t)
		before := [2]int32{atomic.LoadInt32(&u.statusHits), atomic.LoadInt32(&u.checkRunsHits)}
		postWebhook(t, router, tc.event, tc.body)

		w := do(t, router, authedReq("GET", statusTarget, nil))
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a %s event must flush the status snapshot", tc.event)
		w = do(t, router, authedReq("GET", checksTarget, nil))
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a %s event must flush the check-runs snapshot", tc.event)
		assert.Equal(t, before[0]+1, atomic.LoadInt32(&u.statusHits), tc.event)
		assert.Equal(t, before[1]+1, atomic.LoadInt32(&u.checkRunsHits), tc.event)
	}
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
