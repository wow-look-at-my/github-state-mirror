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

// The OWNER-level lookups share the repo route's absorb, rebuild, and row
// space (under sentinel repo values). This pins the two properties that
// sharing must not break: each scope caches independently, and neither
// collides with the repo-level answer for the same login.
func TestCachedOwnerInstallation_ScopesAreIndependent(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		return do(t, router, req)
	}

	targets := []string{
		"/orgs/org1/installation",
		"/users/org1/installation",
		"/repos/org1/repo1/installation",
	}
	for i, target := range targets {
		w := get(target)
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "%s must not be answered by another scope's row", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.installHits))
		assertNoURLKeys(t, w.Body.Bytes())
	}
	for _, target := range targets {
		w := get(target)
		assert.Equal(t, "hit", w.Header().Get(cacheHeader), "%s must serve from its own row", target)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.installHits), "hits must not call upstream")

	// Owner rows carry the same installation id, so the same event flushes them.
	postWebhook(t, router, "installation", `{"action":"suspend","installation":{"id":42}}`)
	for _, target := range targets {
		assert.Equal(t, "miss", get(target).Header().Get(cacheHeader), "%s must be flushed", target)
	}
}

// The authoritative "not installed here" 404 is a cacheable VERDICT: replayed
// under its own status without touching GitHub, and cleared by an installation
// event (verdict rows carry no installation id, so the by-id flush cannot
// reach them — a separate sweep must).
func TestCachedOwnerInstallation_AbsentVerdictCachedAndFlushed(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.install = notInstalled

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/orgs/org1/installation", nil)
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		return do(t, router, req)
	}

	w1 := get()
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.JSONEq(t, `{"message":"Not Found","status":"404"}`, w1.Body.String())
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.installHits))

	w2 := get()
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.installHits), "a cached verdict must not call GitHub")

	// The app gets installed. The delivery names an id no verdict row carries,
	// so only the absent-verdict sweep can clear it.
	u.install = newPullsCacheUpstream().install
	postWebhook(t, router, "installation", `{"action":"created","installation":{"id":42}}`)

	w3 := get()
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "an installation event must clear the absent verdict")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.installHits))
}

// A verdict is scoped like every other row here: per app, per account, per
// question. One account's 404 must never answer another's, and the org and
// user questions stay distinct even for the same login.
func TestCachedOwnerInstallation_AbsentVerdictIsScoped(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.install = notInstalled

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		return do(t, router, req)
	}

	targets := []string{"/orgs/org1/installation", "/users/org1/installation", "/orgs/org2/installation"}
	for i, target := range targets {
		w := get(target)
		require.Equal(t, http.StatusNotFound, w.Code, target)
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "%s must not be answered by another scope's verdict", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.installHits))
	}
	for _, target := range targets {
		assert.Equal(t, "hit", get(target).Header().Get(cacheHeader), "%s must serve its own verdict", target)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.installHits))
}

// An unverifiable bearer forwards unchanged, uncached — GitHub answers the
// caller itself, exactly as on the repo-level route.
func TestCachedOwnerInstallation_UnverifiedForwards(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	req := httptest.NewRequest("GET", "/orgs/org1/installation", nil)
	req.Header.Set("Authorization", "Bearer not-an-app-jwt")

	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.installHits))
}
