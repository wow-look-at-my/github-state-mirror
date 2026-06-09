package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCORS_Preflight verifies an OPTIONS preflight is answered with 204 and the
// CORS headers a browser needs, without requiring a token (browsers do not send
// Authorization on preflight) and without reaching the route's POST handler.
func TestCORS_Preflight(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodOptions, "/graphql", nil)
	req.Header.Set("Origin", "https://wow-look-at-my.github.io")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), "POST")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
}

// TestCORS_ResponseHeader verifies that ordinary responses carry the CORS origin
// header — including auth failures, so the browser can read the 401 and react.
func TestCORS_ResponseHeader(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/user", nil)	// no token -> 401
	req.Header.Set("Origin", "https://wow-look-at-my.github.io")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

// TestCORS_Allowlist verifies that a configured allowlist echoes an allowed
// origin and withholds the header from an unknown one (so the browser blocks it).
func TestCORS_Allowlist(t *testing.T) {
	mw := corsMiddleware([]string{"https://app.example.com"})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	allowed := httptest.NewRequest(http.MethodGet, "/user", nil)
	allowed.Header.Set("Origin", "https://app.example.com")
	wa := httptest.NewRecorder()
	handler.ServeHTTP(wa, allowed)
	assert.Equal(t, "https://app.example.com", wa.Header().Get("Access-Control-Allow-Origin"))

	unknown := httptest.NewRequest(http.MethodGet, "/user", nil)
	unknown.Header.Set("Origin", "https://evil.example.com")
	wu := httptest.NewRecorder()
	handler.ServeHTTP(wu, unknown)
	assert.Equal(t, "", wu.Header().Get("Access-Control-Allow-Origin"))
}
