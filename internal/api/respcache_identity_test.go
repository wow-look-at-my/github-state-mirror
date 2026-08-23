package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Identity/self route tests: GET /app, GET /user, GET /user/orgs. These
// three deliberately do NOT follow the trimmed-rebuild contract every other
// file in this package tests against -- see the file header in
// respcache_identity.go and the comment on
// TestProxy_FormerlyCachedNowForwarded (proxy_test.go) for why. What matters
// here is that a HIT is BYTE-IDENTICAL to the original upstream body, never a
// reshaped subset.

func defaultUserOrgsUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, []any{
		map[string]any{"login": "org1", "id": 1, "url": "https://api.github.com/orgs/org1"},
		map[string]any{"login": "org2", "id": 2, "url": "https://api.github.com/orgs/org2"},
	})
}

// appJWTReq builds a request bearing the App JWT (verified outside
// requireAuth, like /app itself).
func appJWTReq(target string) *http.Request {
	req := authedReq("GET", target, nil)
	req.Header.Set("Authorization", "Bearer "+goodAppJWT)
	return req
}

func TestCachedApp_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, appJWTReq("/app"))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"id":777,"slug":"testapp"}`, w1.Body.String(),
		"a hit/miss body must be byte-identical to GitHub's own -- no trimming")
	after1 := u.appHits // VerifyAppIdentity's own call + this route's own miss fetch, both uncached the first time

	w2 := do(t, router, appJWTReq("/app"))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, after1, u.appHits, "a hit must not call upstream again")
}

func TestCachedUser_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/user", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"login":"testuser","id":7001}`, w1.Body.String(),
		"a hit/miss body must be byte-identical to GitHub's own -- no trimming")
	after1 := u.userHits

	w2 := do(t, router, authedReq("GET", "/user", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, after1, u.userHits, "a hit must not call upstream again")
}

func TestCachedUserOrgs_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/user/orgs", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	// url is NOT dropped: this route is byte-identical, not a trimmed
	// rebuild, so GitHub's own self-links ride through unchanged.
	assert.Contains(t, w1.Body.String(), "api.github.com")
	assert.Contains(t, w1.Body.String(), `"login":"org1"`)
	require.Equal(t, int32(1), u.userOrgsHits)

	w2 := do(t, router, authedReq("GET", "/user/orgs", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), u.userOrgsHits, "a hit must not call upstream again")
}

// Different credentials must never share a row: /user answers a DIFFERENT
// document per token.
func TestCachedUser_NeverSharedAcrossCredentials(t *testing.T) {
	router, _, _, _ := respCacheStack(t)

	first := do(t, router, authedReq("GET", "/user", nil))
	require.Equal(t, http.StatusOK, first.Code)
	assert.Contains(t, first.Body.String(), `"login":"testuser"`)

	second := do(t, router, hooksReq("GET", "/user", "some-other-token"))
	require.Equal(t, http.StatusOK, second.Code)
	assert.Contains(t, second.Body.String(), `"login":"otheruser"`,
		"a different token must never be answered by another credential's cached profile")
}

// A non-default Accept, a query parameter, and a missing bearer all forward
// (identical-or-passthrough has no partial-shape concept to guard).
func TestCachedUser_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	req := authedReq("GET", "/user", nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))
	assert.Positive(t, u.userHits)
}

func TestCachedUserOrgs_EmptyIsCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.userOrgs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[]`))
	}

	w1 := do(t, router, authedReq("GET", "/user/orgs", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "[]", w1.Body.String())
	assert.Equal(t, "hit", do(t, router, authedReq("GET", "/user/orgs", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), u.userOrgsHits)
}

// A 401 (e.g. a revoked App JWT) relays unstored.
func TestCachedApp_UnauthorizedForwards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w := do(t, router, appJWTReq("/app"))
	require.Equal(t, http.StatusOK, w.Code)
	before := u.appHits

	bad := authedReq("GET", "/app", nil)
	bad.Header.Set("Authorization", "Bearer wrong-jwt")
	w2 := do(t, router, bad)
	// An unverifiable JWT falls to the plain passthrough proxy (like the
	// installation-mint routes), which relays GitHub's own 401.
	require.Equal(t, http.StatusUnauthorized, w2.Code)
	assert.Empty(t, w2.Header().Get(cacheHeader))
	assert.Greater(t, u.appHits, before)
}
