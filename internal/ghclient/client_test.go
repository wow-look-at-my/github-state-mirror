package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
)

func TestWithToken_RoundTrip(t *testing.T) {
	ctx := WithToken(context.Background(), "ghp_test123")
	assert.Equal(t, "ghp_test123", tokenFromContext(ctx))
}

func TestTokenFromContext_Empty(t *testing.T) {
	assert.Equal(t, "", tokenFromContext(context.Background()))
}

func testServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewWithBaseURL(srv.URL)
}

func TestResolveTokenIdentity_NoToken(t *testing.T) {
	c := New()
	_, err := c.ResolveTokenIdentity(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token")
}

func TestResolveTokenIdentity_UserCached(t *testing.T) {
	callCount := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		assert.Equal(t, "/user", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "id": 42})
	})

	ctx := WithToken(context.Background(), "test-token")

	ident, err := c.ResolveTokenIdentity(ctx)
	require.NoError(t, err)
	assert.Equal(t, TokenIdentity{IsUser: true, ID: 42, Login: "octocat"}, ident)
	assert.Equal(t, 1, callCount)

	// Second call is served from the per-token cache.
	ident, err = c.ResolveTokenIdentity(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(42), ident.ID)
	assert.Equal(t, 1, callCount) // no additional API call
}

func TestResolveTokenIdentity_BadCredential(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	ctx := WithToken(context.Background(), "bad-token")
	_, err := c.ResolveTokenIdentity(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadCredential)
	assert.Contains(t, err.Error(), "401")
}

// TestResolveTokenIdentity_NotAUserVerdictCached: a plain 403 (no rate-limit
// headers) or a 404 is a DEFINITIVE "not a user" answer (e.g. an installation
// token) — returned as IsUser=false, not an error, and cached per token.
func TestResolveTokenIdentity_NotAUserVerdictCached(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		callCount := 0
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		})

		ctx := WithToken(context.Background(), "ghs_installation-token")
		ident, err := c.ResolveTokenIdentity(ctx)
		require.NoError(t, err, "status %d", status)
		assert.False(t, ident.IsUser)
		assert.Empty(t, ident.Login)

		// The verdict is cached: no second upstream call.
		_, err = c.ResolveTokenIdentity(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, callCount, "status %d", status)
	}
}

// TestResolveTokenIdentity_TransientNotCached: a 5xx is an error and must NOT
// be cached — the next call retries and can succeed without a restart.
func TestResolveTokenIdentity_TransientNotCached(t *testing.T) {
	callCount := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "id": 42})
	})

	ctx := WithToken(context.Background(), "flaky-token")
	_, err := c.ResolveTokenIdentity(ctx)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBadCredential)

	ident, err := c.ResolveTokenIdentity(ctx)
	require.NoError(t, err, "a transient failure must not be cached")
	assert.Equal(t, int64(42), ident.ID)
	assert.Equal(t, 2, callCount)
}

// TestResolveTokenIdentity_RateLimited403IsTransient: a 403 that looks like
// rate limiting is NOT a "not a user" verdict — caching one for a rate-limited
// USER token would mis-partition that user for the process lifetime.
func TestResolveTokenIdentity_RateLimited403IsTransient(t *testing.T) {
	headers := []map[string]string{
		{"X-RateLimit-Remaining": "0"},
		{"Retry-After": "60"},
	}
	for _, hdrs := range headers {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			for k, v := range hdrs {
				w.Header().Set(k, v)
			}
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		})

		ctx := WithToken(context.Background(), "rate-limited-token")
		_, err := c.ResolveTokenIdentity(ctx)
		require.Error(t, err, "headers %v", hdrs)
		assert.NotErrorIs(t, err, ErrBadCredential)
	}
}

// TestResolveTokenIdentity_MalformedUserResponse: a 200 with no id/login is
// malformed and must fail (uncached) rather than partition on garbage.
func TestResolveTokenIdentity_MalformedUserResponse(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})

	ctx := WithToken(context.Background(), "weird-token")
	_, err := c.ResolveTokenIdentity(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing id or login")
}

func TestDoJSON_SetsContentType(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Write([]byte("{}"))
	})

	err := c.doJSON(context.Background(), "GET", "/test", nil, nil)
	require.NoError(t, err)
}

func TestVerifyAppIdentity_Caches(t *testing.T) {
	calls := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/app", r.URL.Path)
		assert.Equal(t, "Bearer jwt-1", r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 42, "slug": "pr-minder"})
	})

	id, err := c.VerifyAppIdentity(context.Background(), "jwt-1")
	require.NoError(t, err)
	assert.Equal(t, int64(42), id.ID)
	assert.Equal(t, "pr-minder", id.Slug)

	// Second call for the same JWT is served from cache (no extra /app call).
	_, err = c.VerifyAppIdentity(context.Background(), "jwt-1")
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestVerifyAppIdentity_RejectsInvalid(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"could not be decoded"}`))
	})

	_, err := c.VerifyAppIdentity(context.Background(), "forged")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

// rateHeaders makes the test server attach X-RateLimit-* headers.
func rateHeaders(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4999")
}

// observed installs a recording rate observer on c and returns the slice of
// identities it was called with.
func observed(c *Client) *[]string {
	var ids []string
	c.SetRateObserver(func(identity, name string, resp *http.Response) {
		ids = append(ids, identity)
	})
	return &ids
}

// TestRateObserver_DoJSONIdentity: doJSON reports every response to the
// observer — labeled by the ctx principal when set, else by the credential's
// shape (JWT -> "app-jwt", other tokens -> a short fingerprint, no token ->
// "anonymous"). The raw token value never reaches the observer.
func TestRateObserver_DoJSONIdentity(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		rateHeaders(w)
		w.Write([]byte("{}"))
	})
	ids := observed(c)

	// ctx principal wins.
	ctx := actor.WithActor(WithToken(context.Background(), "some-token"), "user:42")
	require.NoError(t, c.doJSON(ctx, "GET", "/test", nil, nil))

	// No principal: a raw token becomes a fingerprint label.
	require.NoError(t, c.doJSON(WithToken(context.Background(), "some-token"), "GET", "/test", nil, nil))

	// No principal: a JWT-shaped credential is the app's own.
	require.NoError(t, c.doJSON(WithToken(context.Background(), "eyJx.eyJy.sig"), "GET", "/test", nil, nil))

	// No credential at all.
	require.NoError(t, c.doJSON(context.Background(), "GET", "/test", nil, nil))

	fp := "token:" + Fingerprint("some-token")[:12]
	assert.Equal(t, []string{"user:42", fp, "app-jwt", "anonymous"}, *ids)
	for _, id := range *ids {
		assert.NotContains(t, id, "some-token", "the raw token must never be passed on")
	}
}

// TestRateObserver_ResolveAndVerifyReport: ResolveTokenIdentity and
// VerifyAppIdentity report their responses too (their upstream calls consume
// rate limit like any other).
func TestRateObserver_ResolveAndVerifyReport(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		rateHeaders(w)
		json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "id": 42, "slug": "app-slug"})
	})
	ids := observed(c)

	_, err := c.ResolveTokenIdentity(WithToken(context.Background(), "user-token"))
	require.NoError(t, err)
	_, err = c.VerifyAppIdentity(context.Background(), "h.c.s")
	require.NoError(t, err)

	assert.Equal(t, []string{"token:" + Fingerprint("user-token")[:12], "app-jwt"}, *ids)
}

// TestRateObserver_Unset: a client without an observer must not panic.
func TestRateObserver_Unset(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		rateHeaders(w)
		w.Write([]byte("{}"))
	})
	require.NoError(t, c.doJSON(context.Background(), "GET", "/test", nil, nil))
}

// TestSetExchangeObserver_TimesEveryCall: the transport-level observer sees
// every exchange the client performs — with method, path, status, a real
// non-negative duration, and the credential-shape identity — and a ctx
// principal (with verified name) takes precedence.
func TestSetExchangeObserver_TimesEveryCall(t *testing.T) {
	type obs struct {
		identity, name, method, path string
		status                       int
		dur                          time.Duration
	}
	var mu sync.Mutex
	var got []obs

	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate_limit" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c.SetExchangeObserver(func(identity, name, method, path string, status int, start time.Time, dur time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		require.False(t, start.IsZero())
		got = append(got, obs{identity, name, method, path, status, dur})
	})

	// A tokenless call labels anonymous.
	_ = c.doJSON(context.Background(), "GET", "/meta", nil, nil)
	// A bearer-token call labels by fingerprint shape.
	_ = c.doJSON(WithToken(context.Background(), "ghs_sometoken"), "GET", "/meta", nil, nil)
	// A ctx principal (with name) wins over the credential shape.
	ctx := actor.WithName(actor.WithActor(WithToken(context.Background(), "ghs_other"), "app-installation:481"), "gsm-bot")
	_ = c.doJSON(ctx, "POST", "/rate_limit", nil, nil)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 3, "every exchange is observed")
	assert.Equal(t, "anonymous", got[0].identity)
	assert.Equal(t, "GET", got[0].method)
	assert.Equal(t, "/meta", got[0].path)
	assert.Equal(t, http.StatusOK, got[0].status)
	assert.GreaterOrEqual(t, got[0].dur, time.Duration(0))
	assert.True(t, strings.HasPrefix(got[1].identity, "token:"), "identity %q", got[1].identity)
	assert.Empty(t, got[1].name, "a credential-shape identity never borrows a name")
	assert.Equal(t, "app-installation:481", got[2].identity)
	assert.Equal(t, "gsm-bot", got[2].name)
	assert.Equal(t, http.StatusInternalServerError, got[2].status)
}
