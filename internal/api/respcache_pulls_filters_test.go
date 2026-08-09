package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Open-PR list tests for the query-shape guards, the head/base filters, and
// the two backstops (global reveal sharing, the marker TTL).

func TestCachedPullsList_QueryShapeGuards(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/pulls?sort=updated",          // unknown param
		"/repos/org1/repo1/pulls?state=closed",          // non-open state
		"/repos/org1/repo1/pulls?state=all",             // non-open state
		"/repos/org1/repo1/pulls?page=2",                // beyond page 1
		"/repos/org1/repo1/pulls?per_page=200",          // out of range
		"/repos/org1/repo1/pulls?head=justabranch",      // head without owner:
		"/repos/org1/repo1/pulls?state=open&state=open", // repeated param
		"/repos/org1/repo1/pulls?sort=updated",          // unmodeled ordering
		"/repos/org1/repo1/pulls?base=",                 // empty base filter
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.listHits), target)
	}

	// Passthroughs must not have set the marker: a cacheable shape still misses.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
}

// TestCachedPullsList_HeadFilter: the head=owner:branch shape is served from
// the marker-backed complete set -- a no-match answer is a cached empty array
// (the common case in pr-minder's branch sweeps), while a match as long as
// per_page falls to the pagination guard and misses.
func TestCachedPullsList_HeadFilter(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	// Absorb the complete set.
	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=100", nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	// Filter matching PR #7, roomy per_page: served from state.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:feature&state=open", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	var items []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, float64(7), items[0]["number"])

	// No match: a cached empty array, even at per_page=1.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:no-such-branch&state=open&per_page=1", nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w2.Body.String())

	// A match at per_page=1 fills the page -> pagination guard -> miss.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:feature&state=open&per_page=1", nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestCachedPullsList_BaseFilter: `base` is a pure filter over the open set
// the completeness marker already vouches for, so it is answered from state
// exactly like `head` -- and, like any filtered response, a fetched one never
// SETS the marker.
func TestCachedPullsList_BaseFilter(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	// Absorb the complete set.
	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=100", nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	// Every fixture PR targets main: the filter matches them all, from state.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?base=main&state=open&per_page=100", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	var items []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	assert.NotEmpty(t, items)
	for _, it := range items {
		assert.Equal(t, "main", it["base"].(map[string]any)["ref"])
	}

	// A base nothing targets is a cached empty array, not an upstream call.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?base=release&state=open", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w2.Body.String())

	// head and base compose.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:feature&base=main&state=open", nil))
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader))
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, float64(7), items[0]["number"])

	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits), "filters must be answered from state")
}

// TestCachedPullsList_GlobalSharedViaReveal: the absorbed list is GLOBAL
// truth. A second user pays one reveal probe (their own token proving repo
// access) and then reads the same rebuilt list -- the list itself is fetched
// once, ever.
func TestCachedPullsList_GlobalSharedViaReveal(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100"

	do(t, router, authedReq("GET", target, nil)) // user A: probe + absorb

	reqB := httptest.NewRequest("GET", target, nil)
	reqB.Header.Set("Authorization", "Bearer other-token")
	w := do(t, router, reqB)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "global truth serves every granted principal")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits), "the list is fetched once, ever")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits), "each principal pays exactly one probe")

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits), "grants are remembered; no re-probe")
}

// TestCachedPullsList_MarkerTTLBackstop: with webhooks silent, the marker
// expires and the next read refetches -- a missed delivery cannot serve a
// stale list forever.
func TestCachedPullsList_MarkerTTLBackstop(t *testing.T) {
	router, _, db, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	_, err := db.Exec(`UPDATE pulls_list_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired marker is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestParsePullsListShape_Defaults pins the shape parser's defaults: the bare
// query is cacheable at GitHub's default page size, explicit pr-minder shapes
// parse, and per_page bounds hold.
func TestParsePullsListShape_Defaults(t *testing.T) {
	shape, ok := parsePullsListShape(url.Values{})
	require.True(t, ok)
	assert.Equal(t, pullsDefaultPerPage, shape.perPage)
	assert.Empty(t, shape.head)

	q, _ := url.ParseQuery("state=open&per_page=100&page=1")
	shape, ok = parsePullsListShape(q)
	require.True(t, ok)
	assert.Equal(t, 100, shape.perPage)

	q, _ = url.ParseQuery("head=org1:feature&state=open&per_page=1")
	shape, ok = parsePullsListShape(q)
	require.True(t, ok)
	assert.Equal(t, 1, shape.perPage)
	assert.Equal(t, "org1:feature", shape.head)

	for _, bad := range []string{"per_page=0", "per_page=101", "page=0", "head=:x", "head=x:"} {
		q, _ := url.ParseQuery(bad)
		_, ok := parsePullsListShape(q)
		assert.False(t, ok, bad)
	}
}
