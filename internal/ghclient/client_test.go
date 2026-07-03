package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestGetAuthenticatedUser(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(userResp{Login: "octocat", AvatarURL: "http://avatar", HTMLURL: "http://url"})
	})

	user, err := c.GetAuthenticatedUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "octocat", user.Login)
	assert.Equal(t, "http://avatar", user.AvatarUrl)
}

func TestGetUserOrgs(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]orgResp{
			{Login: "myorg", AvatarURL: "http://avatar", URL: "http://url"},
		})
	})

	orgs, err := c.GetUserOrgs(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, len(orgs))
	assert.Equal(t, "myorg", orgs[0].Login)
}

func TestCompareBranches(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/org/repo/compare/main...feature", r.URL.Path)
		json.NewEncoder(w).Encode(compareResp{AheadBy: 3, BehindBy: 1})
	})

	comp, err := c.CompareBranches(context.Background(), "org", "repo", "main", "feature")
	require.NoError(t, err)
	assert.Equal(t, int64(3), comp.AheadBy)
	assert.Equal(t, int64(1), comp.BehindBy)
}

func TestGetPRFiles(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/org/repo/pulls/42/files", r.URL.Path)
		json.NewEncoder(w).Encode([]prFileResp{
			{Filename: "main.go", Additions: 10, Deletions: 2},
		})
	})

	files, err := c.GetPRFiles(context.Background(), "org", "repo", 42)
	require.NoError(t, err)
	require.Equal(t, 1, len(files))
	assert.Equal(t, "main.go", files[0].Path)
	assert.Equal(t, int64(10), files[0].Additions)
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
