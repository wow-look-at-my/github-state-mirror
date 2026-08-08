package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// docker-updater probes these with no credential. Registering them inside the
// authenticated group -- or not at all -- makes them answer 401 through the
// GitHub passthrough, and the contract reads any non-404 as "implemented", so
// the container would report as permanently unhealthy instead of unconfigured.
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

// The distinction the test above rests on: an unregistered path really does
// answer 401 here rather than 404, so "it would have 404'd anyway" is not a
// reason to skip registering them.
func TestUnregisteredPathAnswers401NotFound(t *testing.T) {
	router, _ := setupTestRouter(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/not-registered", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"an unrouted path falls through to the passthrough proxy, which rejects a tokenless request")
}

// The failing half of health lives with the thing it asserts:
// TestPingFailsOnAClosedDatabase in internal/ghdata.
