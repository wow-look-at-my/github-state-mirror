package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// The same traffic is aggregated into route-shape groups, sorted by total
	// desc and capped: both /graphql calls share one group (1 miss + 1 hit),
	// and /rate_limit groups on its own.
	require.NotEmpty(t, snap.Groups)
	assert.LessOrEqual(t, len(snap.Groups), requestGroupsSnapshotCap)
	byKey := map[string]requestGroupSnapshot{}
	for i, g := range snap.Groups {
		byKey[g.Key] = g
		if i > 0 {
			assert.LessOrEqual(t, g.Total, snap.Groups[i-1].Total, "groups sorted by total desc")
		}
	}
	gq, ok := byKey["POST /graphql"]
	require.True(t, ok, "the graphql group exists")
	assert.GreaterOrEqual(t, gq.Hit, int64(1))
	assert.GreaterOrEqual(t, gq.Miss, int64(1))
	assert.Equal(t, "/graphql", gq.Sample)
	rl, ok := byKey["GET /rate_limit"]
	require.True(t, ok, "the rate_limit group exists")
	assert.GreaterOrEqual(t, rl.Passthrough, int64(1))

	// The stack's real SQLite file is statted end to end: NewRouter threads the
	// DB path through to the dashboard, which reports its on-disk size.
	assert.Positive(t, snap.DBSizeBytes, "the payload reports the SQLite DB's on-disk size")
}

// TestDashboard_Requests_DBSize verifies /api/requests reports the SQLite
// database's on-disk footprint: the DB file plus its -wal sidecar when
// present, with each field omitted — never an error — when its file is
// missing.
func TestDashboard_Requests_DBSize(t *testing.T) {
	svc := configuredAuth(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "known.db")
	require.NoError(t, os.WriteFile(dbPath, make([]byte, 4096), 0o600))
	require.NoError(t, os.WriteFile(dbPath+"-wal", make([]byte, 1536), 0o600))

	// handleRequests reads only auth, reqlog, and dbPath, so the dashboard can
	// be constructed directly with a controlled path.
	get := func(path string) map[string]any {
		d := &dashboard{auth: svc, reqlog: newRequestLog(), dbPath: path}
		req := httptest.NewRequest("GET", "/api/requests", nil)
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := httptest.NewRecorder()
		d.handleRequests(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var m map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
		return m
	}

	// DB file + -wal sidecar: both sizes reported exactly.
	m := get(dbPath)
	assert.Equal(t, float64(4096), m["db_size_bytes"])
	assert.Equal(t, float64(1536), m["db_wal_size_bytes"])

	// No -wal sidecar: the WAL field is omitted, the DB size stays.
	require.NoError(t, os.Remove(dbPath+"-wal"))
	m = get(dbPath)
	assert.Equal(t, float64(4096), m["db_size_bytes"])
	assert.NotContains(t, m, "db_wal_size_bytes", "an absent -wal omits the field")

	// Missing DB file: both fields omitted; the request still succeeds.
	m = get(filepath.Join(dir, "missing.db"))
	assert.NotContains(t, m, "db_size_bytes", "a missing DB omits the field")
	assert.NotContains(t, m, "db_wal_size_bytes")
}

// TestRequests_PassthroughRecordsUpstreamStatus verifies a passthrough records
// the status GitHub returned (e.g. 502), so the dashboard shows whether the
// forwarded call actually succeeded — not just that it was forwarded.
func TestRequests_PassthroughRecordsUpstreamStatus(t *testing.T) {
	svc := configuredAuth(t)
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" { // identity resolution, if reached
			_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
			return
		}
		w.WriteHeader(http.StatusBadGateway) // simulate GitHub 502 on the proxied path
		_, _ = w.Write([]byte("upstream boom"))
	})
	router, _, _, _ := newTestStackWithGitHub(t, svc, gh)

	pass := authedReq("GET", "/some/uncached/path", nil)
	pw := httptest.NewRecorder()
	router.ServeHTTP(pw, pass)
	require.Equal(t, http.StatusBadGateway, pw.Code)

	rl := httptest.NewRequest("GET", "/api/requests", nil)
	rl.AddCookie(mintSession(t, svc, "PazerOP"))
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, rl)
	require.Equal(t, http.StatusOK, rw.Code)

	var snap requestLogSnapshot
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &snap))
	var found bool
	for _, e := range snap.Recent {
		if e.Disposition == DispPassthrough && e.Path == "/some/uncached/path" {
			found = true
			assert.Equal(t, http.StatusBadGateway, e.Status, "passthrough records the upstream status")
		}
	}
	assert.True(t, found, "the passthrough should be in the request log")
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
