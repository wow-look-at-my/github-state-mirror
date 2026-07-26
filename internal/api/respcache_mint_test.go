package api

import (
	"context"
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

// These tests lock the upstream-auth-failure mint invalidation: gsm never
// receives a CONSUMER App's installation webhooks, so a 401/403 from GitHub
// on a proxied call carrying the minted token is the only signal the cached
// mint's grants went stale -- it must drop the mint so the next mint
// refetches, while rate-limit refusals and foreign bearers leave it alone.

// authFailResp builds a minimal upstream response shape for the helper.
func authFailResp(status int, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

func seedMint(t *testing.T, store *ghdata.Store, token string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, store.PutCachedInstallToken(context.Background(), "app:777", ghdata.CachedInstallToken{
		InstallationID: "42", BodyHash: "h", Token: token,
		TokenExpiresAt: now.Add(time.Hour).UTC().Format(time.RFC3339),
	}, now, now.Add(30*time.Minute)))
}

func mintCached(t *testing.T, store *ghdata.Store) bool {
	t.Helper()
	_, ok, err := store.GetCachedInstallToken(context.Background(), "app:777", "42", "h", time.Now())
	require.NoError(t, err)
	return ok
}

func TestInvalidateMintOnAuthFailure(t *testing.T) {
	_, store, _, _ := respCacheStack(t)

	// 403 permission verdict: the mint drops.
	seedMint(t, store, "ghs_tok1")
	invalidateMintOnAuthFailure(context.Background(), store, "ghs_tok1", authFailResp(403, nil))
	assert.False(t, mintCached(t, store), "a 403 on the minted token must drop the cached mint")

	// 401 drops too (revoked/expired-out-of-band token).
	seedMint(t, store, "ghs_tok1")
	invalidateMintOnAuthFailure(context.Background(), store, "ghs_tok1", authFailResp(401, nil))
	assert.False(t, mintCached(t, store))

	// Rate-limit-shaped 403s are NOT permission verdicts: the mint stays.
	seedMint(t, store, "ghs_tok1")
	invalidateMintOnAuthFailure(context.Background(), store, "ghs_tok1",
		authFailResp(403, map[string]string{"X-Ratelimit-Remaining": "0"}))
	assert.True(t, mintCached(t, store), "a primary rate limit must not drop the mint")
	invalidateMintOnAuthFailure(context.Background(), store, "ghs_tok1",
		authFailResp(403, map[string]string{"Retry-After": "60"}))
	assert.True(t, mintCached(t, store), "a secondary rate limit must not drop the mint")

	// Non-installation bearers and success statuses leave the cache alone.
	invalidateMintOnAuthFailure(context.Background(), store, "ghp_usertoken", authFailResp(403, nil))
	invalidateMintOnAuthFailure(context.Background(), store, "ghs_other", authFailResp(200, nil))
	assert.True(t, mintCached(t, store))
}

// The wiring end to end through the passthrough proxy: mint through the
// router (cached), fail an unmodeled proxied call carrying the minted
// token, and the next mint refetches instead of serving the stale cache.
func TestMintInvalidatedByProxiedAuthFailure(t *testing.T) {
	var mintCount, forbidCount atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"id": 777, "slug": "testapp"}`))
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			n := mintCount.Add(1)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token": "ghs_minted%d", "expires_at": %q}`,
				n, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.URL.Path == "/repos/org1/repo1/collaborators":
			forbidCount.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), upstream)

	mint := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		return do(t, router, req)
	}

	w1 := mint()
	require.Equal(t, http.StatusCreated, w1.Code)
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))
	require.Equal(t, "hit", mint().Header().Get(cacheHeader), "the mint must cache before the failure")
	require.Equal(t, int32(1), mintCount.Load())

	// An unmodeled path (the router's NotFound passthrough proxy) that the
	// upstream 404s -- NOT an auth failure -- leaves the mint cached.
	call := httptest.NewRequest("GET", "/repos/org1/repo1/topics", nil)
	call.Header.Set("Authorization", "Bearer ghs_minted1")
	require.Equal(t, http.StatusNotFound, do(t, router, call).Code)
	assert.Equal(t, "hit", mint().Header().Get(cacheHeader), "a 404 must not drop the mint")

	// The upstream 403s a proxied call carrying the minted token: the mint
	// drops and the next mint refetches.
	forbidden := httptest.NewRequest("GET", "/repos/org1/repo1/collaborators", nil)
	forbidden.Header.Set("Authorization", "Bearer ghs_minted1")
	require.Equal(t, http.StatusForbidden, do(t, router, forbidden).Code)
	require.Equal(t, int32(1), forbidCount.Load())

	w2 := mint()
	require.Equal(t, http.StatusCreated, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "an upstream 403 on the token must invalidate the mint")
	assert.Equal(t, int32(2), mintCount.Load())
}
