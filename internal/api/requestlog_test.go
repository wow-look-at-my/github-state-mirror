package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestLog_RecordAndSnapshot(t *testing.T) {
	l := newRequestLog()
	l.record("app:1", "POST", "/graphql", DispMiss)
	l.record("app:1", "POST", "/graphql", DispHit)
	l.record("token:x", "GET", "/rate_limit", DispPassthrough)

	snap := l.snapshot(10)
	assert.Equal(t, int64(3), snap.Total)
	assert.Equal(t, int64(1), snap.ByDisposition[DispMiss])
	assert.Equal(t, int64(1), snap.ByDisposition[DispHit])
	assert.Equal(t, int64(1), snap.ByDisposition[DispPassthrough])

	require.Len(t, snap.Recent, 3)
	assert.Equal(t, "/rate_limit", snap.Recent[0].Path, "recent is newest-first")
	assert.Equal(t, DispPassthrough, snap.Recent[0].Disposition)
	assert.Equal(t, "/graphql", snap.Recent[2].Path)
}

func TestRequestLog_CapAndLimit(t *testing.T) {
	l := newRequestLog()
	for i := 0; i < requestLogRecentCap+50; i++ {
		l.record("a", "GET", "/p", DispHit)
	}
	all := l.snapshot(0)
	assert.Equal(t, int64(requestLogRecentCap+50), all.Total, "total counts every record")
	assert.Len(t, all.Recent, requestLogRecentCap, "recent ring is capped")

	limited := l.snapshot(10)
	assert.Len(t, limited.Recent, 10, "snapshot honors the limit")
}

func TestJwtIssuer(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	assert.Equal(t, "12345", jwtIssuer("h."+enc(`{"iss":"12345","exp":1}`)+".sig"))
	assert.Equal(t, "12345", jwtIssuer("h."+enc(`{"iss":12345}`)+".sig"), "numeric iss")
	assert.Equal(t, "", jwtIssuer("not-a-jwt"))
	assert.Equal(t, "", jwtIssuer("only.two"))
	assert.Equal(t, "", jwtIssuer("h."+enc(`not json`)+".s"))
}

func TestCallerLabel(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"42"}`))

	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Mirror-Identity", "h."+payload+".s")
	assert.Equal(t, "app:42", callerLabel(r), "identity header -> app:<id>")

	r2 := httptest.NewRequest("GET", "/x", nil)
	r2.Header.Set("Authorization", "Bearer some-token")
	assert.True(t, strings.HasPrefix(callerLabel(r2), "token:"), "bearer -> token:<fingerprint>")

	r3 := httptest.NewRequest("GET", "/x", nil)
	assert.Equal(t, "anonymous", callerLabel(r3))
}

// TestDashboard_Requests_Admin drives real requests through the router and
// verifies the dashboard's /api/requests reports their cache dispositions: the
// first org-repos query is a miss, the second a hit, and an uncached path a
// passthrough.
func TestDashboard_Requests_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	orgQuery := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	for i := 0; i < 2; i++ { // miss, then hit
		req := authedReq(http.MethodPost, "/graphql", strings.NewReader(orgQuery))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
	// Uncached path -> passthrough (the test upstream answers any path 200).
	passReq := authedReq("GET", "/rate_limit", nil)
	pw := httptest.NewRecorder()
	router.ServeHTTP(pw, passReq)
	require.Equal(t, http.StatusOK, pw.Code)

	req := httptest.NewRequest("GET", "/api/requests", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var snap requestLogSnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
	assert.GreaterOrEqual(t, snap.ByDisposition[DispMiss], int64(1), "first org query should be a cache miss")
	assert.GreaterOrEqual(t, snap.ByDisposition[DispHit], int64(1), "second org query should be a cache hit")
	assert.GreaterOrEqual(t, snap.ByDisposition[DispPassthrough], int64(1), "/rate_limit should be a passthrough")
	assert.GreaterOrEqual(t, len(snap.Recent), 3)
}

func TestDashboard_Requests_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/requests", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_Requests_Unauthenticated(t *testing.T) {
	router, _, _ := newTestStack(t, configuredAuth(t))

	req := httptest.NewRequest("GET", "/api/requests", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
