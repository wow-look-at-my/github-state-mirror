package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
)

// TestGHECanonicalPath pins the mapping itself, including the paths that must
// keep their own meaning. /api/timeline is the dashboard's own route: a rewrite
// there would take the Timeline chart off the air.
func TestGHECanonicalPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/api/v3/user", "/user", true},
		{"/api/v3/repos/o/r/pulls/1", "/repos/o/r/pulls/1", true},
		{"/api/v3", "/", true},
		{"/api/v3/", "/", true},
		{"/api/graphql", "/graphql", true},
		{"/user", "", false},
		{"/graphql", "", false},
		{"/api/timeline", "", false},
		{"/api/cache/data", "", false},
		{"/api/v3x/user", "", false},
		{"/repos/o/api/v3", "", false},
	}
	for _, c := range cases {
		got, ok := gheCanonicalPath(c.in)
		assert.Equal(t, c.ok, ok, "%s rewritten?", c.in)
		if c.ok {
			assert.Equal(t, c.want, got, "%s rewrites to", c.in)
		}
	}
}

// TestGHECompat_RESTPrefixHitsTheCachedRoute is the whole point: gh sends
// /api/v3/user to a non-github.com host, and that must reach the same cached
// route as /user rather than the passthrough proxy.
func TestGHECompat_RESTPrefixHitsTheCachedRoute(t *testing.T) {
	router, _ := setupTestRouter(t)

	plain := httptest.NewRecorder()
	router.ServeHTTP(plain, authedReq(http.MethodGet, "/user", nil))
	require.Equal(t, http.StatusOK, plain.Code)

	prefixed := httptest.NewRecorder()
	router.ServeHTTP(prefixed, authedReq(http.MethodGet, "/api/v3/user", nil))

	assert.Equal(t, plain.Code, prefixed.Code)
	assert.JSONEq(t, plain.Body.String(), prefixed.Body.String())
}

// TestGHECompat_GraphQLAlias verifies gh's enterprise GraphQL path reaches the
// cached org-repos assembler, not the proxy.
func TestGHECompat_GraphQLAlias(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
	})
	router, store, _, _ := newTestStackWithGitHub(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), gh)
	seedOrgTruth(t, store, testUserActor, "my-org", nil, nil)

	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	req := authedReq(http.MethodPost, "/api/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"organization"`)
}

// TestGHECompat_PassthroughDropsThePrefix covers the uncached routes. GitHub
// has no /api/v3 namespace, so a forwarded prefix is a miss every time.
func TestGHECompat_PassthroughDropsThePrefix(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		writeGitHubJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
	})
	router, _, _, _ := newTestStackWithGitHub(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), gh)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedReq(http.MethodGet, "/api/v3/gists", nil))
	require.Equal(t, http.StatusOK, w.Code)

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, paths, "/gists", "the proxy must forward the api.github.com spelling")
	for _, p := range paths {
		assert.NotContains(t, p, "/api/v3", "no upstream call may carry the enterprise prefix")
	}
}

// TestGHECompat_StillNeedsAToken: the rewrite grants nothing. The prefixed
// spelling meets the same auth gate as the plain spelling.
func TestGHECompat_StillNeedsAToken(t *testing.T) {
	router, _ := setupTestRouter(t)
	for _, target := range []string{"/api/v3/user", "/api/v3/repos/o/r/pulls", "/api/v3/gists"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "GET %s without a token", target)
	}
}
