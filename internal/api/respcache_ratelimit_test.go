package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// TestRateLimit_ServedFromObservedState verifies GET /rate_limit answers from
// the ratemeter's passively observed X-RateLimit-* headers -- fed here by a
// cached-route MISS on the SAME resolved identity ("user:<id>", since both
// requests carry testToken) -- proving the identity keying lines up between
// requireAuth's principal and what fetchUpstream's meter.Observe recorded.
func TestRateLimit_ServedFromObservedState(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "base64", "size": 5,
			"name": "f", "path": "f", "content": "aGVsbG8=\n", "sha": "s",
		})
	}

	// A cached-route MISS feeds the meter under this token's resolved
	// identity before /rate_limit is ever asked.
	miss := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/f", nil))
	require.Equal(t, http.StatusOK, miss.Code)

	w := do(t, router, authedReq("GET", "/rate_limit", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "served from state, never a miss")

	var resp ghclient.RateLimitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.Resources, "core")
	assert.Equal(t, 5000, resp.Resources["core"].Limit)
	assert.Equal(t, 4321, resp.Resources["core"].Remaining)
	assert.Equal(t, int64(9999999999), resp.Resources["core"].Reset)
}

// TestRateLimit_NeverCallsUpstream: the route must be answered ENTIRELY from
// memory. A fake GitHub that fails the test on any GET /rate_limit call
// proves the route never fetches it, even on the very first request for a
// credential the meter has never observed (still 200 with an empty
// resources object — see TestRateLimit_EmptyWhenNoObservation — never a
// fetch attempt).
func TestRateLimit_NeverCallsUpstream(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate_limit" {
			t.Fatal("GET /rate_limit must never reach upstream")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	w := do(t, router, authedReq("GET", "/rate_limit", nil))
	require.Equal(t, http.StatusOK, w.Code)
}

// TestRateLimit_EmptyWhenNoObservation: a credential the meter has never
// observed gets an honestly empty `resources` object, never stale or
// fabricated numbers -- the same "omit, don't guess" stance
// ratemeter.Store.ObservationsFor documents.
func TestRateLimit_EmptyWhenNoObservation(t *testing.T) {
	router, _, _, _ := respCacheStack(t)
	w := do(t, router, authedReq("GET", "/rate_limit", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))

	var resp ghclient.RateLimitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Resources)
}

// TestRateLimit_UnmodeledShapePassesThrough: GET /rate_limit takes no query
// parameters; an unexpected one is not this route's shape to answer, so it
// forwards uncached rather than silently ignoring the parameter.
func TestRateLimit_UnmodeledShapePassesThrough(t *testing.T) {
	router, _, _, _ := respCacheStack(t)
	w := do(t, router, authedReq("GET", "/rate_limit?foo=bar", nil))
	assert.Empty(t, w.Header().Get(cacheHeader), "an unmodeled query shape must pass through")
}
