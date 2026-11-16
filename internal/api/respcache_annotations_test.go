package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Check-run annotations tests.

const annotationsTarget = "/repos/org1/repo1/check-runs/555/annotations?per_page=100"

// answersStack answers the annotations read with status, counting each.
func answersStack(t *testing.T) (http.Handler, *int32, *int) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var annCalls int32
	status := http.StatusOK
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/annotations") {
			return false
		}
		atomic.AddInt32(&annCalls, 1)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration","status":"403"}`))
			return true
		}
		writeGitHubJSON(w, []any{map[string]any{
			"path": "a.go", "start_line": 3, "end_line": 3, "start_column": nil, "end_column": nil,
			"annotation_level": "failure", "title": "build", "message": "boom", "raw_details": "",
			"blob_href": "https://github.com/org1/repo1/blob/x/a.go",
		}})
		return true
	}
	return router, &annCalls, &status
}

func TestCachedAnnotations_MissHitAndCheckRunFlush(t *testing.T) {
	router, calls, _ := answersStack(t)

	w1 := do(t, router, authedReq("GET", annotationsTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString([]any{map[string]any{
		"path": "a.go", "start_line": 3, "end_line": 3, "start_column": nil, "end_column": nil,
		"annotation_level": "failure", "title": "build", "message": "boom", "raw_details": "",
	}}), w1.Body.String())
	w2 := do(t, router, authedReq("GET", annotationsTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())

	// Another run's delivery leaves the row; this run's delivery drops it.
	for _, id := range []int{556, 555} {
		postWebhookJSON(t, router, "check_run", map[string]any{
			"action": "completed", "repository": fixtureRepo(),
			"check_run": map[string]any{"id": id, "head_sha": shaTip, "name": "build", "status": "completed", "conclusion": "failure"},
		})
		want := "hit"
		if id == 555 {
			want = "miss"
		}
		assert.Equal(t, want, do(t, router, authedReq("GET", annotationsTarget, nil)).Header().Get(cacheHeader), "after run %d", id)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestCachedAnnotations_ForbiddenIsPerCaller(t *testing.T) {
	router, calls, status := answersStack(t)
	*status = http.StatusForbidden
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", annotationsTarget, nil))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

