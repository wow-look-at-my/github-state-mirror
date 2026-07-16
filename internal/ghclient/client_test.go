package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// retryTestServer is testServer with zero retry backoff, so the transient
// retries under test never really sleep.
func retryTestServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	c := testServer(t, handler)
	c.SetRetryBackoff([]time.Duration{0})
	return c
}

// TestDoJSON_RetriesTransient502: a single 502 is retried and the second
// attempt's result is returned (the request -- body included -- is resent).
func TestDoJSON_RetriesTransient502(t *testing.T) {
	calls := 0
	var bodies []string
	c := retryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	var out struct {
		OK bool `json:"ok"`
	}
	err := c.doJSON(context.Background(), "POST", "/graphql", strings.NewReader(`{"query":"q"}`), &out)
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.Equal(t, 2, calls, "one retry after the 502")
	assert.Equal(t, []string{`{"query":"q"}`, `{"query":"q"}`}, bodies, "every attempt resends the full body")
}

// TestDoJSON_PersistentTransientFailsAfterAttempts: a persistently-502ing
// upstream fails after exactly doJSONAttempts requests, surfacing the status.
func TestDoJSON_PersistentTransientFailsAfterAttempts(t *testing.T) {
	calls := 0
	c := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Bad Gateway"))
	})

	err := c.doJSON(context.Background(), "GET", "/test", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
	assert.Equal(t, doJSONAttempts, calls)
}

// TestDoJSON_AuthoritativeStatusNotRetried: 4xx answers (other than 429) are
// authoritative -- the reveal layer and deny-cache semantics depend on them --
// so they must fail on the first attempt.
func TestDoJSON_AuthoritativeStatusNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		calls := 0
		c := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(status)
		})
		err := c.doJSON(context.Background(), "GET", "/test", nil, nil)
		require.Error(t, err, "status %d", status)
		assert.Equal(t, 1, calls, "status %d must not be retried", status)
	}
}

// flakyTransport fails the first failFirst round trips with a network error,
// then delegates to the real transport.
type flakyTransport struct {
	calls     int
	failFirst int
	next      http.RoundTripper
	err       error
}

func (tr *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls++
	if tr.calls <= tr.failFirst {
		return nil, tr.err
	}
	return tr.next.RoundTrip(req)
}

// TestDoJSON_RetriesNetworkError: a transient network failure is retried like
// a 502.
func TestDoJSON_RetriesNetworkError(t *testing.T) {
	c := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	})
	tr := &flakyTransport{failFirst: 1, next: http.DefaultTransport, err: fmt.Errorf("connection reset by peer")}
	c.httpClient.Transport = tr

	require.NoError(t, c.doJSON(context.Background(), "GET", "/test", nil, nil))
	assert.Equal(t, 2, tr.calls)
}

// TestDoJSON_ContextCancellationNotRetried: the caller's own cancellation is
// never a transient failure -- retrying it only delays the exit.
func TestDoJSON_ContextCancellationNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := &flakyTransport{failFirst: doJSONAttempts, err: fmt.Errorf("Get \"x\": %w", context.Canceled)}
	c := NewWithBaseURL("http://unused.invalid")
	c.SetRetryBackoff([]time.Duration{0})
	c.httpClient.Transport = tr
	cancel()

	err := c.doJSON(ctx, "GET", "/test", nil, nil)
	require.Error(t, err)
	assert.Equal(t, 1, tr.calls, "a canceled context must not be retried")
}

// TestRetryDelay_RetryAfterFloorAndCap: a parseable Retry-After raises the
// backoff floor but is capped so a huge value can't wedge a deadline-bounded
// fetch; absent/garbage headers leave the configured backoff alone.
func TestRetryDelay_RetryAfterFloorAndCap(t *testing.T) {
	resp := func(retryAfter string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		if retryAfter != "" {
			r.Header.Set("Retry-After", retryAfter)
		}
		return r
	}
	assert.Equal(t, time.Duration(0), retryAfterDelay(nil))
	assert.Equal(t, time.Duration(0), retryAfterDelay(resp("")))
	assert.Equal(t, time.Duration(0), retryAfterDelay(resp("soon")))
	assert.Equal(t, 2*time.Second, retryAfterDelay(resp("2")))
	assert.Equal(t, retryAfterCap, retryAfterDelay(resp("3600")), "a huge Retry-After is capped")

	c := New()
	assert.Equal(t, 500*time.Millisecond, c.retryDelay(2, nil))
	assert.Equal(t, 2*time.Second, c.retryDelay(3, nil), "the last backoff entry repeats")
	assert.Equal(t, 2*time.Second, c.retryDelay(4, nil))
	assert.Equal(t, 5*time.Second, c.retryDelay(2, resp("5")), "Retry-After raises the floor")
	assert.Equal(t, retryAfterCap, c.retryDelay(2, resp("3600")))

	c.SetRetryBackoff([]time.Duration{time.Minute})
	assert.Equal(t, time.Minute, c.retryDelay(2, resp("5")), "a larger configured backoff wins over Retry-After")
}

// TestRateObserver_SeesRetriedResponses: the passive rate meter must observe
// EVERY response the client receives, retried 502s included.
func TestRateObserver_SeesRetriedResponses(t *testing.T) {
	calls := 0
	c := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		rateHeaders(w)
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("{}"))
	})
	ids := observed(c)

	require.NoError(t, c.doJSON(actor.WithActor(context.Background(), "user:42"), "GET", "/test", nil, nil))
	assert.Equal(t, []string{"user:42", "user:42"}, *ids, "both attempts' responses are observed")
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
