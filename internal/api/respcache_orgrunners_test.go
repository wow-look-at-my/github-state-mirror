package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Org self-hosted-runners route tests.

func defaultOrgRunnersUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, map[string]any{
		"total_count": 1,
		"runners": []any{
			map[string]any{
				"id": 1, "name": "wow-linux-1", "os": "linux", "status": "online", "busy": false,
				"labels": []any{map[string]any{"id": 1, "name": "self-hosted", "type": "read-only"}},
			},
		},
	})
}

const orgRunnersTarget = "/orgs/org1/actions/runners"

func TestCachedOrgRunners_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", orgRunnersTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.orgRunnersHits))
	// Verbatim: the labels array rides through unchanged, since a trim would likely guess wrong.
	assert.Contains(t, w1.Body.String(), `"labels":[{"id":1,"name":"self-hosted","type":"read-only"}]`)
	assert.Contains(t, w1.Body.String(), `"status":"online"`)

	w2 := do(t, router, authedReq("GET", orgRunnersTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.orgRunnersHits), "a hit must not call upstream")
}

// Never shared across credentials: an admin-scoped read must not leak
// between two callers.
func TestCachedOrgRunners_NeverSharedAcrossCredentials(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, hooksReq("GET", orgRunnersTarget, "tok-a")).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, hooksReq("GET", orgRunnersTarget, "tok-b")).Header().Get(cacheHeader),
		"an admin-only answer must never be replayed to a credential GitHub has not answered")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.orgRunnersHits))
}

// Shape guards: pages are separate rows, and a non-default Accept forwards.
func TestCachedOrgRunners_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", orgRunnersTarget+"?per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", orgRunnersTarget+"?per_page=50&page=2", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, authedReq("GET", orgRunnersTarget+"?per_page=50&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.orgRunnersHits))

	req := authedReq("GET", orgRunnersTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.orgRunnersHits))
}

// A 403 (not an admin) relays unstored, never a cached verdict.
func TestCachedOrgRunners_ForbiddenRelayedUnstored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.orgRunners = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Must have admin rights to Organization."}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", orgRunnersTarget, nil))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.orgRunnersHits))
	}
}
