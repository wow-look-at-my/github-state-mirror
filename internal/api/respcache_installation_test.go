package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Repo-installation route tests (GET /repos/{owner}/{repo}/installation);
// shared fixtures (pullsCacheStack, ...) live in respcache_pulls_test.go.

// TestCachedRepoInstallation_HitAndFlush: the App-JWT-authed repo-installation
// lookup is cached per app, rebuilt without URL fields, flushed by
// installation events, and unverifiable bearers pass through.
func TestCachedRepoInstallation_HitAndFlush(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/installation"

	get := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		return do(t, router, req)
	}

	w1 := get(goodAppJWT)
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{
		"id": 42,
		"account": {"login": "org1", "type": "Organization"},
		"repository_selection": "all",
		"app_id": 777, "app_slug": "testapp", "target_type": "Organization"
	}`, w1.Body.String())

	w2 := get(goodAppJWT)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.installHits))

	// installation event for id 42 -> flush -> refetch.
	postWebhook(t, router, "installation_repositories", `{"action":"added","installation":{"id":42}}`)
	w3 := get(goodAppJWT)
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "installation events must flush the cache")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.installHits))

	// A bearer that does not verify as an App JWT is forwarded, uncached.
	for i := 3; i <= 4; i++ {
		w := get("not-an-app-jwt")
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.installHits))
	}
}

// TestCachedRepoInstallation_RecordsAppIdentity: this route self-verifies its
// App JWT outside requireAuth, so it must record the verified app:<id> ->
// slug mapping itself — otherwise the principal never reaches
// actor_identities and the dashboard shows "(unknown)".
func TestCachedRepoInstallation_RecordsAppIdentity(t *testing.T) {
	router, store, _, _ := pullsCacheStack(t)

	req := httptest.NewRequest("GET", "/repos/org1/repo1/installation", nil)
	req.Header.Set("Authorization", "Bearer "+goodAppJWT)
	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)

	ids, err := store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Equal(t, "app:777", ids[0].Actor)
	assert.Equal(t, "testapp", ids[0].Login)
}
