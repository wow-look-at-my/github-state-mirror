package api

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func closedPullsUpstream(u *pullsCacheUpstream) {
	u.list = func(w http.ResponseWriter, r *http.Request) {
		pr5 := upstreamPR(5, "closed", "Merged PR", "old", shaCommit, "2026-06-01T10:00:00Z")
		pr5["closed_at"] = "2026-06-02T10:00:00Z"
		pr5["merged_at"] = "2026-06-02T10:00:00Z"
		servePRJSON(w, []any{pr5})
	}
}

// A closed page is a stored snapshot: miss, then a hit with the same bytes,
// whatever the query order.
func TestCachedPullsSnapshot_MissHit(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	closedPullsUpstream(u)

	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=closed&per_page=100", nil))
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	var items []map[string]any
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, "closed", items[0]["state"])
	assert.Equal(t, "2026-06-02T10:00:00Z", items[0]["merged_at"])
	assert.Equal(t, "2026-06-02T10:00:00Z", items[0]["closed_at"])

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?page=1&per_page=100&state=closed", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	// state=all is a different row.
	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=all&per_page=100", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// One PR's delivery does not state a page's membership, so the pages go.
func TestCachedPullsSnapshot_PullRequestDeliveryFlushes(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	closedPullsUpstream(u)
	target := "/repos/org1/repo1/pulls?state=closed"
	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "pull_request", prEvent("closed", upstreamPR(7, "closed", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

func TestCachedPullsSnapshot_LabelDeliveryFlushes(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	closedPullsUpstream(u)
	target := "/repos/org1/repo1/pulls?state=all"
	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	postWebhookJSON(t, router, "label", map[string]any{
		"action": "edited", "label": map[string]any{"name": "bug", "color": "ffffff"}, "repository": fixtureRepo(),
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

func TestCachedPullsSnapshot_ErrorsRelayUnstored(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusMovedPermanently} {
		router, _, _, u := pullsCacheStack(t)
		u.list = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		}
		for range 2 {
			w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=closed", nil))
			assert.Equal(t, status, w.Code)
			assert.Empty(t, w.Header().Get(cacheHeader))
		}
		assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits), "status %d", status)
	}
}

// An absent PR number is a stored verdict, shared by the JSON and diff
// reads, until a pull_request delivery names that number.
func TestCachedPull_AbsentVerdict(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/x","status":"404"}`))
	}
	target := "/repos/org1/repo1/pulls/9"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())

	req := authedReq("GET", target, nil)
	req.Header.Set("Accept", "application/vnd.github.diff")
	w3 := do(t, router, req)
	assert.Equal(t, http.StatusNotFound, w3.Code)
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	postWebhook(t, router, "pull_request", prEvent("opened", upstreamPR(9, "open", "New", "nine", shaTip, "2026-07-05T10:00:00Z")))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
}
