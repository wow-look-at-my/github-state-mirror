package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
)

func testAuth() *auth.Service {
	return auth.New(auth.Config{SessionKey: []byte("test-session-key")})
}

// TestProxy_ForwardsUnknownRESTPath verifies that a path the mirror does not
// cache is transparently forwarded to GitHub: method, path, query, token, and
// body are passed through. The upstream status/body/headers come back. CORS
// headers are applied because chi runs Use middleware around the NotFound route.
func TestProxy_ForwardsUnknownRESTPath(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"resources":{"core":{"remaining":4999}}}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := authedReq("GET", "/meta?foo=bar", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Upstream status and body are returned verbatim.
	assert.Equal(t, http.StatusTeapot, w.Code)
	assert.Contains(t, w.Body.String(), `"remaining":4999`)

	// Upstream received the forwarded request unchanged.
	assert.Equal(t, "GET", gotMethod)
	assert.Equal(t, "/meta", gotPath)
	assert.Equal(t, "foo=bar", gotQuery)
	assert.Equal(t, "Bearer "+testToken, gotAuth, "caller's token must be forwarded")

	// Upstream response headers and CORS both pass through to the client.
	assert.Equal(t, "4999", w.Header().Get("X-RateLimit-Remaining"))
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

// TestProxy_DeduplicatesCORS verifies that when GitHub returns its own CORS
// headers (it does — Access-Control-Allow-Origin: *). The forwarded response
// carries exactly Access-Control-Allow-Origin (the mirror's) so browsers do
// not reject it for having multiple values, while Expose-Headers is preserved.
func TestProxy_DeduplicatesCORS(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.Header().Set("Access-Control-Expose-Headers", "X-RateLimit-Remaining, Link")
		_, _ = w.Write([]byte(`{}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := authedReq("GET", "/meta", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	acao := w.Header().Values("Access-Control-Allow-Origin")
	require.Len(t, acao, 1, "exactly one Access-Control-Allow-Origin (the mirror's)")
	assert.Equal(t, "*", acao[0])
	// Expose-Headers is not stripped and exposes the union; see docs/testing/test-harness.md.
	exposed := strings.Join(w.Header().Values("Access-Control-Expose-Headers"), ", ")
	assert.Contains(t, exposed, "Link", "GitHub's own Expose-Headers must survive")
	assert.Contains(t, exposed, cacheHeader, "the mirror's own headers must be exposed too")
}

// TestProxy_RequiresToken verifies the passthrough is not an open relay: a
// request without an Authorization header is rejected with and never
// reaches GitHub.
func TestProxy_RequiresToken(t *testing.T) {
	var upstreamHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := httptest.NewRequest("GET", "/rate_limit", nil) // no token
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, int32(0), atomic.LoadInt32(&upstreamHits), "must not forward a tokenless request")
}

// TestProxy_AnonymousPassthroughCoversThePublicSurfaceOnly: a tokenless read on
// an unrouted path is forwarded, and GitHub sees no credential of the mirror's.
// A tokenless write is refused and never forwarded; see anonymousPublicOnly.
func TestProxy_AnonymousPassthroughCoversThePublicSurfaceOnly(t *testing.T) {
	var upstreamHits int32
	var gotAuth string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	read := httptest.NewRecorder()
	router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/gists", nil))
	require.Equal(t, http.StatusOK, read.Code, "a public read is forwarded anonymously")
	assert.Empty(t, gotAuth, "an anonymous forward carries no credential")

	write := httptest.NewRecorder()
	router.ServeHTTP(write, httptest.NewRequest(http.MethodPost, "/gists", nil))
	assert.Equal(t, http.StatusUnauthorized, write.Code, "a tokenless write is refused")
	assert.Equal(t, int32(1), atomic.LoadInt32(&upstreamHits), "a refused write must not reach GitHub")
}

// TestProxy_Uncached verifies the passthrough does not cache: repeated requests
// each reach GitHub.
func TestProxy_Uncached(t *testing.T) {
	var upstreamHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	for i := 0; i < 3; i++ {
		req := authedReq("GET", "/repos/o/r/releases", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&upstreamHits), "each call must reach GitHub uncached")
}

// TestProxy_MethodNotAllowedForwarded verifies that a known path hit with an
// unregistered method (e.g. POST /user, which only exists as GET) is forwarded
// rather than 'd. The mirror is a complete GitHub surface.
func TestProxy_MethodNotAllowedForwarded(t *testing.T) {
	var gotMethod, gotPath string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := authedReq("POST", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "POST", gotMethod)
	assert.Equal(t, "/user", gotPath)
}

// TestProxy_PreflightNotForwarded verifies a CORS preflight on an unknown path
// is answered locally () and never forwarded to GitHub.
func TestProxy_PreflightNotForwarded(t *testing.T) {
	var upstreamHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := httptest.NewRequest(http.MethodOptions, "/rate_limit", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, int32(0), atomic.LoadInt32(&upstreamHits), "preflight must not be forwarded")
}
