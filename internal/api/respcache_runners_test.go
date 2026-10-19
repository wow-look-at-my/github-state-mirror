package api

import (
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the single-runner reads and the repo runner listing.

const (
	orgRunnerTarget   = "/orgs/org1/actions/runners/77"
	repoRunnersTarget = "/repos/org1/repo1/actions/runners"
	repoRunnerTarget  = "/repos/org1/repo1/actions/runners/77"
)

var (
	singleRunnerPath = regexp.MustCompile(`/actions/runners/(\d+)$`)
	repoRunnersPath  = regexp.MustCompile(`^/repos/[^/]+/[^/]+/actions/runners$`)
)

func runnerObject(id int64, busy bool) map[string]any {
	return map[string]any{
		"id": id, "name": "wow-linux-" + strconv.FormatInt(id, 10), "os": "linux", "status": "online", "busy": busy,
		"labels": []any{map[string]any{"id": 1, "name": "self-hosted", "type": "read-only"}},
	}
}

// runnerStack answers the runner paths with counters; a later runner is absent.
func runnerStack(t *testing.T) (http.Handler, *respCacheUpstream, *int32, *int32) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var singleHits, listHits int32
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if m := singleRunnerPath.FindStringSubmatch(r.URL.Path); m != nil {
			atomic.AddInt32(&singleHits, 1)
			id, _ := strconv.ParseInt(m[1], 10, 64)
			if id == 404 {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				writeGitHubJSONBody(t, w, map[string]any{"message": "Not Found", "status": "404"})
				return true
			}
			writeGitHubJSON(w, runnerObject(id, false))
			return true
		}
		if repoRunnersPath.MatchString(r.URL.Path) {
			atomic.AddInt32(&listHits, 1)
			writeGitHubJSON(w, map[string]any{"total_count": 1, "runners": []any{runnerObject(77, false)}})
			return true
		}
		return false
	}
	return router, u, &singleHits, &listHits
}

func writeGitHubJSONBody(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	_, _ = w.Write([]byte(mustJSONString(v)))
}

func TestCachedSingleRunner_MissThenHit(t *testing.T) {
	for _, target := range []string{orgRunnerTarget, repoRunnerTarget} {
		router, _, single, _ := runnerStack(t)
		w1 := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w1.Code, target)
		assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
		w2 := do(t, router, authedReq("GET", target, nil))
		assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
		assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
		assert.Equal(t, int32(1), atomic.LoadInt32(single), "a hit must not call upstream: %s", target)
	}
}

func TestCachedSingleRunner_NotFoundStored(t *testing.T) {
	router, _, single, _ := runnerStack(t)
	target := "/repos/org1/repo1/actions/runners/404"
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(single))
}

func TestCachedRunners_NeverSharedAcrossCredentials(t *testing.T) {
	router, _, single, list := runnerStack(t)
	for _, tok := range []string{"tok-a", "tok-b"} {
		assert.Equal(t, "miss", do(t, router, hooksReq("GET", repoRunnerTarget, tok)).Header().Get(cacheHeader), tok)
		assert.Equal(t, "miss", do(t, router, hooksReq("GET", repoRunnersTarget, tok)).Header().Get(cacheHeader), tok)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(single))
	assert.Equal(t, int32(2), atomic.LoadInt32(list))
}

func TestCachedRepoRunners_ListingWarmsSingle(t *testing.T) {
	router, _, single, list := runnerStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", repoRunnersTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", repoRunnersTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", repoRunnerTarget, nil)).Header().Get(cacheHeader),
		"a listed runner must answer its own single read")
	assert.Equal(t, int32(1), atomic.LoadInt32(list))
	assert.Equal(t, int32(0), atomic.LoadInt32(single))
}

func TestCachedOrgRunners_ListingWarmsSingle(t *testing.T) {
	router, u, single, _ := runnerStack(t)
	u.orgRunners = func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, map[string]any{"total_count": 1, "runners": []any{runnerObject(77, false)}})
	}
	require.Equal(t, "miss", do(t, router, authedReq("GET", orgRunnersTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", orgRunnerTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(0), atomic.LoadInt32(single))
}

func TestCachedRunners_ShapeGuards(t *testing.T) {
	router, _, single, list := runnerStack(t)
	assert.Empty(t, do(t, router, authedReq("GET", repoRunnerTarget+"?x=1", nil)).Header().Get(cacheHeader))
	req := authedReq("GET", repoRunnerTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Empty(t, do(t, router, authedReq("GET", repoRunnersTarget+"?page=11", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(single))
	assert.Equal(t, int32(1), atomic.LoadInt32(list))

	require.Equal(t, "miss", do(t, router, authedReq("GET", repoRunnersTarget+"?per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", repoRunnersTarget+"?per_page=50&page=2", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", repoRunnersTarget+"?page=1&per_page=50", nil)).Header().Get(cacheHeader))
}

// A job on the runner moves its busy flag, so the delivery that names the
// runner drops the answers that hold it, for every credential.
func TestCachedRunners_WorkflowJobFlushesRunner(t *testing.T) {
	router, u, _, _ := runnerStack(t)
	seed := func() {
		for _, target := range []string{orgRunnerTarget, repoRunnerTarget, repoRunnersTarget, orgRunnersTarget, "/repos/org1/repo1/actions/runners/78"} {
			do(t, router, authedReq("GET", target, nil))
			require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader), target)
		}
	}
	seed()
	before := atomic.LoadInt32(&u.orgRunnersHits)
	postWebhookJSON(t, router, "workflow_job", map[string]any{
		"action": "in_progress",
		"workflow_job": map[string]any{
			"id": 1, "run_id": 9001, "name": "build", "status": "in_progress", "head_sha": "0123456789abcdef0123456789abcdef01234567",
			"runner_id": 77, "runner_name": "wow-linux-77",
		},
		"repository": fixtureRepo(),
	})
	for _, target := range []string{orgRunnerTarget, repoRunnerTarget, repoRunnersTarget, orgRunnersTarget} {
		assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader), target)
	}
	assert.Equal(t, before+1, atomic.LoadInt32(&u.orgRunnersHits))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/repos/org1/repo1/actions/runners/78", nil)).Header().Get(cacheHeader),
		"another runner's answer must survive")
}
