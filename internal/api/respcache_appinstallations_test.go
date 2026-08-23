package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// App-installations listing route tests.

func defaultAppInstallationsUpstream(w http.ResponseWriter, r *http.Request) {
	writeGitHubJSON(w, []any{
		map[string]any{
			"id": 1001, "app_id": 777, "app_slug": "testapp",
			"account":              map[string]any{"login": "org1", "type": "Organization"},
			"target_type":          "Organization",
			"repository_selection": "all",
			"permissions":          map[string]any{"contents": "read", "metadata": "read"},
			"events":               []any{"push", "pull_request"},
			"created_at":           "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
		},
	})
}

const appInstallationsTarget = "/app/installations"

func TestCachedAppInstallations_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, appJWTReq(appInstallationsTarget))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.appInstallationsHits))
	// Verbatim: permissions/events (a trim would likely have dropped) ride
	// through unchanged.
	assert.Contains(t, w1.Body.String(), `"permissions":{"contents":"read","metadata":"read"}`)
	assert.Contains(t, w1.Body.String(), `"events":["push","pull_request"]`)

	w2 := do(t, router, appJWTReq(appInstallationsTarget))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.appInstallationsHits), "a hit must not call upstream")
}

// The load-bearing property: an `installation` delivery names its own
// app_id, and the flush targets exactly that app -- never every app.
func TestCachedAppInstallations_WebhookFlushesOwningAppOnly(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, appJWTReq(appInstallationsTarget)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, appJWTReq(appInstallationsTarget)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "installation", map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id": 1002, "app_id": 777, "account": map[string]any{"login": "org2"},
		},
	})

	assert.Equal(t, "miss", do(t, router, appJWTReq(appInstallationsTarget)).Header().Get(cacheHeader),
		"a delivery naming this app's id must flush its cached pages")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.appInstallationsHits))
}

// A delivery for a DIFFERENT app's installation must not flush this app's
// pages.
func TestCachedAppInstallations_WebhookForOtherAppLeavesRowAlone(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, appJWTReq(appInstallationsTarget)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, appJWTReq(appInstallationsTarget)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "installation", map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id": 5000, "app_id": 999, "account": map[string]any{"login": "unrelated"},
		},
	})

	assert.Equal(t, "hit", do(t, router, appJWTReq(appInstallationsTarget)).Header().Get(cacheHeader),
		"a different app's delivery must not touch this app's cached pages")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.appInstallationsHits))
}

// Shape guards: pages are separate rows, and a non-default Accept forwards.
func TestCachedAppInstallations_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, appJWTReq(appInstallationsTarget+"?per_page=50&page=1")).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, appJWTReq(appInstallationsTarget+"?per_page=50&page=2")).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, appJWTReq(appInstallationsTarget+"?per_page=50&page=1")).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.appInstallationsHits))

	req := appJWTReq(appInstallationsTarget)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.appInstallationsHits))
}

// An unverifiable JWT falls to the plain passthrough proxy.
func TestCachedAppInstallations_UnverifiedJWTForwards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	req := authedReq("GET", appInstallationsTarget, nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	w := do(t, router, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))
	assert.Positive(t, u.appInstallationsHits)
}
