package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tags listing tests.

const tagsTarget = "/repos/org1/repo1/tags"

const zeroOID = "0000000000000000000000000000000000000000"

func tagObject(name, sha string) map[string]any {
	return map[string]any{
		"name":        name,
		"zipball_url": "https://api.github.com/repos/org1/repo1/zipball/refs/tags/" + name,
		"tarball_url": "https://api.github.com/repos/org1/repo1/tarball/refs/tags/" + name,
		"commit":      map[string]any{"sha": sha, "url": "https://api.github.com/repos/org1/repo1/commits/" + sha},
		"node_id":     "REF_" + name,
	}
}

// tagsStack answers the tags listing with status and body, counting calls.
func tagsStack(t *testing.T) (http.Handler, *int32, *int, *[]any) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var calls int32
	status := http.StatusOK
	page := []any{tagObject("v1", shaBase), tagObject("v0", shaTree1)}
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/tags") {
			return false
		}
		atomic.AddInt32(&calls, 1)
		switch status {
		case http.StatusOK:
			writeGitHubJSON(w, page)
		case http.StatusUnauthorized:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("Bad credentials"))
		default:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
		}
		return true
	}
	return router, &calls, &status, &page
}

func TestCachedTags_MissAbsorbHit(t *testing.T) {
	router, calls, _, _ := tagsStack(t)

	w1 := do(t, router, authedReq("GET", tagsTarget+"?per_page=100&page=1", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString([]any{
		map[string]any{"name": "v1", "commit": map[string]any{"sha": shaBase}, "node_id": "REF_v1"},
		map[string]any{"name": "v0", "commit": map[string]any{"sha": shaTree1}, "node_id": "REF_v0"},
	}), w1.Body.String())

	// The same page under a reordered query is the same row.
	w2 := do(t, router, authedReq("GET", tagsTarget+"?page=1&per_page=100", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	// Another page is its own row.
	assert.Equal(t, "miss", do(t, router, authedReq("GET", tagsTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestCachedTags_Verdicts(t *testing.T) {
	router, calls, status, _ := tagsStack(t)
	*status = http.StatusNotFound
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", tagsTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code, "request %d", i)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	*status = http.StatusUnauthorized
	for i := int32(2); i <= 3; i++ {
		w := do(t, router, authedReq("GET", tagsTarget+"?page=2", nil))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, i, atomic.LoadInt32(calls))
	}
}

func TestCachedTags_ShapeGuardsPassThrough(t *testing.T) {
	router, calls, _, _ := tagsStack(t)
	for _, target := range []string{
		tagsTarget + "?per_page=101", tagsTarget + "?page=11", tagsTarget + "?sort=name",
		tagsTarget + "?page=1&page=2",
	} {
		assert.Empty(t, do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader), target)
	}
	req := authedReq("GET", tagsTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(5), atomic.LoadInt32(calls))
}

// A push that moves a tag rewrites its entry in place; nothing is refetched.
func TestCachedTags_TagMoveApplies(t *testing.T) {
	router, calls, _, _ := tagsStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", tagsTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/tags/v1", "before": shaBase, "after": shaTip, "forced": true,
		"repository": fixtureRepo(), "commits": []any{},
	})
	// A branch push does not touch the tags listing.
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(), "commits": []any{},
	})

	w := do(t, router, authedReq("GET", tagsTarget, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.JSONEq(t, mustJSONString([]any{
		map[string]any{"name": "v1", "commit": map[string]any{"sha": shaTip}, "node_id": "REF_v1"},
		map[string]any{"name": "v0", "commit": map[string]any{"sha": shaTree1}, "node_id": "REF_v0"},
	}), w.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

// A created or deleted tag changes the set, which the payload cannot place
// on a page.
func TestCachedTags_MembershipChangesFlush(t *testing.T) {
	router, calls, _, _ := tagsStack(t)
	deliveries := []struct {
		event   string
		payload map[string]any
	}{
		{"push", map[string]any{"ref": "refs/tags/v2", "before": zeroOID, "after": shaTip, "repository": fixtureRepo(), "commits": []any{}}},
		{"push", map[string]any{"ref": "refs/tags/v1", "before": shaBase, "after": zeroOID, "repository": fixtureRepo(), "commits": []any{}}},
		{"delete", map[string]any{"ref": "v0", "ref_type": "tag", "repository": fixtureRepo()}},
		{"repository", map[string]any{"action": "renamed", "repository": fixtureRepo()}},
	}
	for i, d := range deliveries {
		require.Equal(t, "miss", do(t, router, authedReq("GET", tagsTarget, nil)).Header().Get(cacheHeader), "before delivery %d", i)
		require.Equal(t, "hit", do(t, router, authedReq("GET", tagsTarget, nil)).Header().Get(cacheHeader), "before delivery %d", i)
		postWebhookJSON(t, router, d.event, d.payload)
	}
	assert.Equal(t, "miss", do(t, router, authedReq("GET", tagsTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(len(deliveries)+1), atomic.LoadInt32(calls))
}
