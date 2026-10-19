package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// docker-updater probes these with no credential. Registering them inside the
// authenticated group -- or not at all -- makes them answer through the GitHub
// passthrough, and the contract reads any non- as "implemented". The container
// would report as permanently unhealthy instead of unconfigured.
func TestUpdateCheckEndpointsAnswerWithoutAToken(t *testing.T) {
	router, _ := setupTestRouter(t)

	for _, path := range []string{
		"/.well-known/docker-updater/health",
		"/.well-known/docker-updater/pre-update",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, path)
	}
}

// The distinction the test above rests on: the mirror itself answers nothing
// for an unregistered path. A tokenless probe is forwarded to GitHub carrying no
// credential, so the prober reads GitHub's own answer. GitHub serves nothing
// under /.well-known/, so an unregistered probe still reads as unconfigured.
func TestUnregisteredPathAnswers401NotFound(t *testing.T) {
	var mu sync.Mutex
	forwarded := map[string]string{}
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		forwarded[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/not-registered", nil))
	require.Equal(t, http.StatusNotFound, rec.Code, "an unrouted probe reads the upstream answer")

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, forwarded, "/.well-known/not-registered", "the unregistered path is forwarded, not answered here")
	assert.Empty(t, forwarded["/.well-known/not-registered"], "an anonymous forward carries no credential")
}
