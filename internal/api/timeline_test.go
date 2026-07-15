package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// timelinePayload mirrors timelineResponse for decoding in tests.
type timelinePayload struct {
	Events         []reqtimeline.Event `json:"events"`
	MaxID          uint64              `json:"max_id"`
	RetentionStart string              `json:"retention_start"`
	Now            string              `json:"now"`
}

// TestTimeline_AdminGated: /api/timeline follows the /api/requests admin
// model — 401 anonymous, 403 signed-in non-admin, 200 admin.
func TestTimeline_AdminGated(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))

	// Anonymous: 401.
	w := do(t, s.router, httptest.NewRequest(http.MethodGet, "/api/timeline", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Signed-in non-admin: 403.
	req := httptest.NewRequest(http.MethodGet, "/api/timeline", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w = do(t, s.router, req)
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Admin: 200 with the payload shape.
	s.timeline.RecordWebhook(time.Now(), 3*time.Millisecond, "push", "", "d-1", "o/r", "applied")
	req = httptest.NewRequest(http.MethodGet, "/api/timeline", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = do(t, s.router, req)
	require.Equal(t, http.StatusOK, w.Code)
	var got timelinePayload
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Events, 1)
	assert.Equal(t, reqtimeline.KindWebhook, got.Events[0].Kind)
	assert.Equal(t, "⇐ push", got.Events[0].Lane)
	assert.Equal(t, uint64(1), got.MaxID)
	assert.NotEmpty(t, got.RetentionStart)
	assert.NotEmpty(t, got.Now)
}

// TestTimeline_SinceCursor: ?since=<id> pages incrementally, and a garbage
// cursor is a 400.
func TestTimeline_SinceCursor(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))
	s.timeline.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d-1", "o/r", "applied")
	s.timeline.RecordWebhook(time.Now(), time.Millisecond, "pull_request", "opened", "d-2", "o/r", "applied")

	fetch := func(target string) timelinePayload {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := do(t, s.router, req)
		require.Equal(t, http.StatusOK, w.Code)
		var got timelinePayload
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		return got
	}

	full := fetch("/api/timeline")
	require.Len(t, full.Events, 2)
	require.Equal(t, uint64(2), full.MaxID)

	// Cursor past the first event: only the second comes back.
	page := fetch("/api/timeline?since=1")
	require.Len(t, page.Events, 1)
	assert.Equal(t, uint64(2), page.Events[0].ID)

	// Cursor at the frontier: empty page, MaxID still reported.
	page = fetch("/api/timeline?since=2")
	assert.Empty(t, page.Events)
	assert.Equal(t, uint64(2), page.MaxID)

	// Garbage cursor: 400.
	req := httptest.NewRequest(http.MethodGet, "/api/timeline?since=banana", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := do(t, s.router, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestTimeline_WebhookDeliveryRecorded: a verified delivery through the
// router lands in the timeline ring with its real fields and a measured
// duration — and the webhook response itself is unchanged.
func TestTimeline_WebhookDeliveryRecorded(t *testing.T) {
	s := newFullTestStack(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))

	body := `{"action":"opened","pull_request":{"number":7,"head":{"sha":"beef"},"base":{"ref":"main"}},"repository":{"name":"repo1","owner":{"login":"org1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-42")
	req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, []byte(body)))
	w := do(t, s.router, req)
	require.Less(t, w.Code, 300, "delivery must succeed: %s", w.Body.String())

	snap := s.timeline.Snapshot(0)
	require.Len(t, snap.Events, 1)
	e := snap.Events[0]
	assert.Equal(t, reqtimeline.KindWebhook, e.Kind)
	assert.Equal(t, "⇐ pull_request", e.Lane)
	assert.Equal(t, "pull_request", e.EventType)
	assert.Equal(t, "opened", e.Action)
	assert.Equal(t, "delivery-42", e.DeliveryID)
	assert.Equal(t, "org1/repo1", e.Repo)
	assert.NotEmpty(t, e.Disposition)
	assert.False(t, e.Start.IsZero(), "start must be stamped")
	assert.GreaterOrEqual(t, e.DurMs, int64(0), "duration is a real measurement")

	// An unverified delivery leaves no trace.
	bad := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	bad.Header.Set("X-GitHub-Event", "pull_request")
	bad.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w = do(t, s.router, bad)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Len(t, s.timeline.Snapshot(0).Events, 1)
}

// TestTimeline_PassthroughRecorded: a request the passthrough proxy forwards
// is timed into the ring under its normalized route lane.
func TestTimeline_PassthroughRecorded(t *testing.T) {
	s := newFullTestStack(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"login":"testuser","id":7001}`))
	}))

	w := do(t, s.router, authedReq(http.MethodGet, "/repos/org1/repo1/git/refs/heads/feature", nil))
	require.Equal(t, http.StatusOK, w.Code)

	snap := s.timeline.Snapshot(0)
	require.Len(t, snap.Events, 1)
	e := snap.Events[0]
	assert.Equal(t, reqtimeline.KindRequest, e.Kind)
	assert.Equal(t, http.MethodGet, e.Method)
	assert.Equal(t, "/repos/{owner}/{repo}/git/refs/heads/…", e.Route)
	assert.Equal(t, "GET /repos/{owner}/{repo}/git/refs/heads/…", e.Lane)
	assert.Equal(t, DispPassthrough, e.Disposition)
	assert.Equal(t, http.StatusOK, e.Status)
	// The passthrough sits outside requireAuth, so the caller labels as a
	// token fingerprint (the request log's exact behavior).
	assert.True(t, strings.HasPrefix(e.Actor, "token:"), "actor %q", e.Actor)
	assert.GreaterOrEqual(t, e.DurMs, int64(0))
}

// TestTimeline_MissFetchRecorded: a cached route's miss goes through
// fetchUpstream, which times the real GitHub round-trip into the ring — the
// reveal probe deliberately records nothing (only the two choke points feed
// the chart).
func TestTimeline_MissFetchRecorded(t *testing.T) {
	u := newRespCacheUpstream()
	s := newFullTestStack(t, testAuth(), u.handler())

	w := do(t, s.router, authedReq(http.MethodGet, "/repos/org1/repo1/contents/.github/cfg.jsonc", nil))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "miss", w.Header().Get("X-GSM-Cache"))

	snap := s.timeline.Snapshot(0)
	require.Len(t, snap.Events, 1, "exactly the contents fetch — the probe records nothing")
	e := snap.Events[0]
	assert.Equal(t, reqtimeline.KindRequest, e.Kind)
	assert.Equal(t, DispMiss, e.Disposition)
	assert.Equal(t, "/repos/{owner}/{repo}/contents/{path}", e.Route)
	assert.Equal(t, http.StatusOK, e.Status)
	assert.Equal(t, testUserActor, e.Actor, "cached routes run inside requireAuth, so the principal labels the event")

	// A HIT makes no upstream call and records nothing new.
	w = do(t, s.router, authedReq(http.MethodGet, "/repos/org1/repo1/contents/.github/cfg.jsonc", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "hit", w.Header().Get("X-GSM-Cache"))
	assert.Len(t, s.timeline.Snapshot(0).Events, 1)
}
