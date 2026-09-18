package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Tests for the pieces the dashboard renders rather than routes: the
// freshness/refresh grouping helpers, and the admin-only jobs view.

func TestGroupKinds(t *testing.T) {
	rows := []dbgen.ActorFreshnessByKindRow{
		{ResourceKind: "org_repos", FetchState: "fresh", Count: 2, LastFetched: "2026-01-01T00:00:00Z"},
		{ResourceKind: "org_repos", FetchState: "stale", Count: 1, LastFetched: "2026-02-01T00:00:00Z"},
		{ResourceKind: "org_repos", FetchState: "error", Count: 1, LastFetched: "2026-02-02T00:00:00Z"},
		{ResourceKind: "pr_files", FetchState: "fresh", Count: 5, LastFetched: []byte("2026-03-01T00:00:00Z")},
	}
	errRows := []dbgen.ActorErrorMessagesByKindRow{
		{ResourceKind: "org_repos", ResourceKey: "wow-look-at-my", ErrorMessage: sql.NullString{String: "github api POST /graphql: 502", Valid: true}},
	}
	out := groupKinds(rows, errRows)
	require.Len(t, out, 2)
	assert.Equal(t, "org_repos", out[0].Kind)
	assert.Equal(t, int64(2), out[0].States["fresh"])
	assert.Equal(t, int64(1), out[0].States["stale"])
	assert.Equal(t, "2026-02-02T00:00:00Z", out[0].LastFetched) // max across states
	assert.Equal(t, "github api POST /graphql: 502", out[0].Error)
	assert.Equal(t, "wow-look-at-my", out[0].ErrorKey)
	assert.Equal(t, "2026-03-01T00:00:00Z", out[1].LastFetched) // []byte coerced
	assert.Empty(t, out[1].Error)
}

func TestShortAndTime(t *testing.T) {
	assert.Equal(t, "0123456789ab", shortFingerprint("0123456789abcdef"))
	assert.Equal(t, "short", shortFingerprint("short"))
	// Structured actors are never truncated — truncating would drop id digits.
	assert.Equal(t, "user:12345678901", shortFingerprint("user:12345678901"))
	assert.Equal(t, "app-installation:123", shortFingerprint("app-installation:123"))
	assert.Equal(t, "app:99", shortFingerprint("app:99"))

	assert.Equal(t, "x", asTimeString("x"))
	assert.Equal(t, "y", asTimeString([]byte("y")))
	assert.Equal(t, "", asTimeString(nil))
	assert.Equal(t, "", asTimeString(42))
}

// TestToRecent_SurfacesError verifies a failed refresh carries its captured
// error_message through to the dashboard response, while a successful does
// not — so the UI can show *why* a refresh errored, not just that it did.
func TestToRecent_SurfacesError(t *testing.T) {
	logs := []dbgen.CacheRefreshLog{
		{
			ResourceKind: "org_repos", ResourceKey: "wow-look-at-my", TriggeredBy: "lazy",
			StartedAt:    "2024-01-01T00:00:00Z",
			CompletedAt:  sql.NullString{String: "2024-01-01T00:00:01Z", Valid: true},
			Success:      sql.NullInt64{Int64: 0, Valid: true},
			ErrorMessage: sql.NullString{String: "github graphql: 403 Forbidden", Valid: true},
		},
		{
			ResourceKind: "user", ResourceKey: "self", TriggeredBy: "lazy",
			StartedAt:   "2024-01-01T00:00:00Z",
			CompletedAt: sql.NullString{String: "2024-01-01T00:00:01Z", Valid: true},
			Success:     sql.NullInt64{Int64: 1, Valid: true},
		},
	}
	out := toRecent(logs)
	require.Len(t, out, 2)

	assert.Equal(t, "error", out[0].Status)
	assert.Equal(t, "github graphql: 403 Forbidden", out[0].Error, "errored refresh must carry its failure reason")

	assert.Equal(t, "success", out[1].Status)
	assert.Empty(t, out[1].Error, "successful refresh has no error detail")
}

// seedJob writes workflow job row (global — no actor scoping).
func seedJob(t *testing.T, store *ghdata.Store, id int64, name, status, conclusion, startedAt, completedAt string) {
	t.Helper()
	require.NoError(t, store.RecordWorkflowJob(context.Background(), ghdata.WorkflowJob{
		Owner: "o", Repo: "r", JobID: id, RunID: 5, RunAttempt: 1,
		Name: name, WorkflowName: "CI", Status: status, Conclusion: conclusion,
		HeadSHA: "cafe", HeadBranch: "main", HTMLURL: "https://github.com/o/r/actions/runs/5/job/1",
		StartedAt: startedAt, CompletedAt: completedAt,
	}))
}

// jobTime is relative to now: a job completed past workflowJobRetention is pruned on write.
func jobTime(hoursAgo int) string {
	return time.Now().Add(-time.Duration(hoursAgo) * time.Hour).UTC().Format(time.RFC3339)
}

func TestDashboard_Jobs_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	seedJob(t, store, 1, "done-old", "completed", "success", jobTime(73), jobTime(72))
	seedJob(t, store, 2, "done-new", "completed", "failure", jobTime(49), jobTime(48))
	seedJob(t, store, 3, "running", "in_progress", "", jobTime(24), "")

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp jobsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Jobs, 3)
	// Running, then completed newest-completed.
	assert.Equal(t, "running", resp.Jobs[0].Name)
	assert.Equal(t, "done-new", resp.Jobs[1].Name)
	assert.Equal(t, "done-old", resp.Jobs[2].Name)
	assert.Equal(t, "failure", resp.Jobs[1].Conclusion)
	assert.Equal(t, int64(2), resp.Jobs[1].JobID)
	assert.Equal(t, "o", resp.Jobs[0].Owner)
	assert.Equal(t, "r", resp.Jobs[0].Repo)
}

func TestDashboard_Jobs_Limit(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	seedJob(t, store, 1, "a", "completed", "success", jobTime(73), jobTime(72))
	seedJob(t, store, 2, "b", "completed", "success", jobTime(49), jobTime(48))
	seedJob(t, store, 3, "c", "in_progress", "", jobTime(24), "")

	// limit honored.
	req := httptest.NewRequest("GET", "/api/jobs?limit=1", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var resp jobsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Jobs, 1)
	assert.Equal(t, "c", resp.Jobs[0].Name)

	// A limit beyond the cap is clamped (still; returns what exists).
	req = httptest.NewRequest("GET", "/api/jobs?limit=99999", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	resp = jobsResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Jobs, 3)

	// Garbage limit is a.
	req = httptest.NewRequest("GET", "/api/jobs?limit=zero", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestDashboard_Jobs_LimitCapEnforced seeds more rows than the cap and
// verifies the response is clamped to jobsMaxLimit even when a larger limit is
// requested.
func TestDashboard_Jobs_LimitCapEnforced(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	for i := 1; i <= jobsMaxLimit+10; i++ {
		seedJob(t, store, int64(i), "j", "in_progress", "", "2026-07-03T10:00:00Z", "")
	}

	req := httptest.NewRequest("GET", "/api/jobs?limit=10000", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp jobsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Jobs, jobsMaxLimit)
}

func TestDashboard_Jobs_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedPrincipal(t, store, db, "user:100", "octocat")

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_Jobs_Unauthenticated(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
