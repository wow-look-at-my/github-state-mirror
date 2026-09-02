package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The install-token route tests plus the cross-route cache assertions (the row
// cap, the request-log dispositions). The contents route has its own file,
// respcache_contents_test.go; the fake GitHub and the shared assertions they
// all use live in respcache_fixture_test.go.

// TestCachedInstallToken_HitVariantsAndFlush covers the token-mint cache: the
// same app+installation+body serves from cache until expiry; a different body
// is a different token (its own mint); an installation event flushes.
func TestCachedInstallToken_HitVariantsAndFlush(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/app/installations/42/access_tokens"

	mint := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		return do(t, router, req)
	}

	// Miss: minted upstream, absorbed, rebuilt.
	w1 := mint("")
	require.Equal(t, http.StatusCreated, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	var m1 map[string]interface{}
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &m1))
	assert.Equal(t, "ghs_minted1", m1["token"])
	assert.Equal(t, "all", m1["repository_selection"])
	assert.Equal(t, map[string]interface{}{"contents": "read", "metadata": "read"}, m1["permissions"])

	// Hit: same app+installation+body -> the SAME minted token, no upstream call.
	w2 := mint("")
	require.Equal(t, http.StatusCreated, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.mintHits))

	// A different (permissions-subset) body is a DIFFERENT token: fresh mint.
	w3 := mint(`{"permissions":{"contents":"read"}}`)
	require.Equal(t, http.StatusCreated, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.mintHits), "a body variant must mint its own token")

	// ...and is cached under its own key.
	w4 := mint(`{"permissions": {"contents": "read"}}`) // same body modulo whitespace
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader), "canonicalized bodies share a key")
	assert.Equal(t, w3.Body.String(), w4.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.mintHits))

	// installation event for id -> flush -> next mint refetches.
	postWebhook(t, router, "installation", `{"action":"suspend","installation":{"id":42}}`)
	w5 := mint("")
	require.Equal(t, http.StatusCreated, w5.Code)
	assert.Equal(t, "miss", w5.Header().Get(cacheHeader), "installation event must flush cached mints")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.mintHits))
}

// TestCachedInstallToken_ExpiryBufferRemint: a token whose expires_at is
// inside the safety buffer is served but never cached, so every request
// re-mints — a cached mint always has usable lifetime left.
func TestCachedInstallToken_ExpiryBufferRemint(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.tokenExpiry = time.Now().Add(5 * time.Minute) // < -minute buffer

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		w := do(t, router, req)
		require.Equal(t, http.StatusCreated, w.Code)
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.mintHits), "a near-expiry token must re-mint every time")
	}
}

// TestCachedInstallToken_NonAppJWTPassthrough: a caller whose bearer does not
// verify as an App JWT is forwarded to GitHub unchanged and never cached.
func TestCachedInstallToken_NonAppJWTPassthrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer not-an-app-jwt")
		w := do(t, router, req)
		require.Equal(t, http.StatusCreated, w.Code, "GitHub's own answer passes through")
		assert.Empty(t, w.Header().Get(cacheHeader), "passthrough responses carry no cache marker")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.mintHits), "unverified callers are never served from cache")
	}
}

// TestRespCache_PruneCap: each cache table is LRU-pruned on write, so it can
// never grow past the row cap.
func TestRespCache_PruneCap(t *testing.T) {
	prev := ghdata.CacheMaxRows
	ghdata.CacheMaxRows = 3
	t.Cleanup(func() { ghdata.CacheMaxRows = prev })

	router, _, db, _ := respCacheStack(t)
	for i := 0; i < 6; i++ {
		w := do(t, router, authedReq("GET", fmt.Sprintf("/repos/org1/repo1/contents/file-%d.txt", i), nil))
		require.Equal(t, http.StatusOK, w.Code)
	}

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM contents_cache`).Scan(&count))
	assert.LessOrEqual(t, count, 3, "prune-on-write must cap the table at CacheMaxRows")
	assert.Greater(t, count, 0)
}

// TestCachedRoutes_RequestLogDispositions: the dashboard's request log must
// reflect the cached routes — a miss records `miss`, a repeat records `hit` —
// so the hit/miss counters finally show real numbers for REST traffic.
func TestCachedRoutes_RequestLogDispositions(t *testing.T) {
	svc := configuredAuth(t)
	u := newRespCacheUpstream()
	router, _, _, _ := newTestStackWithGitHub(t, svc, u.handler())

	target := "/repos/org1/repo1/contents/cfg.jsonc"
	for i := 0; i < 2; i++ { // miss, then hit
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code)
	}

	req := httptest.NewRequest("GET", "/api/requests", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)

	var snap requestLogSnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
	assert.GreaterOrEqual(t, snap.ByDisposition[DispMiss], int64(1), "first contents read records a miss")
	assert.GreaterOrEqual(t, snap.ByDisposition[DispHit], int64(1), "second contents read records a hit")

	var sawMiss, sawHit bool
	for _, e := range snap.Recent {
		if e.Path == target && e.Disposition == DispMiss {
			sawMiss = true
			assert.Equal(t, http.StatusOK, e.Status, "a miss records the upstream status")
		}
		if e.Path == target && e.Disposition == DispHit {
			sawHit = true
		}
	}
	assert.True(t, sawMiss && sawHit, "both dispositions must appear in the log")
}

// TestCachedInstallToken_RecordsAppIdentity: the token-mint route self-verifies
// its App JWT outside requireAuth, so it records the verified app:<id> -> slug
// mapping itself (the same gap as the repo-installation route). An
// unverifiable bearer records nothing.
func TestCachedInstallToken_RecordsAppIdentity(t *testing.T) {
	router, store, _, _ := respCacheStack(t)

	bad := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
	bad.Header.Set("Authorization", "Bearer not-an-app-jwt")
	_ = do(t, router, bad)
	ids, err := store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids, "an unverified bearer must not record an identity")

	req := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+goodAppJWT)
	w := do(t, router, req)
	require.Equal(t, http.StatusCreated, w.Code)

	ids, err = store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Equal(t, "app:777", ids[0].Actor)
	assert.Equal(t, "testapp", ids[0].Login)
}
