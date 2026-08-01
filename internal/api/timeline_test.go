package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// eventsWhere filters a snapshot's events by disposition, so assertions stay
// robust as more sources record onto the shared ring (e.g. requireAuth's
// ghclient /user resolution).
func eventsWhere(snap reqtimeline.Snapshot, disposition string) []reqtimeline.Event {
	var out []reqtimeline.Event
	for _, e := range snap.Events {
		if e.Disposition == disposition {
			out = append(out, e)
		}
	}
	return out
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
	got := decodeTimelineV1(t, w.Body.Bytes())
	require.Len(t, got.events, 1)
	assert.Equal(t, reqtimeline.KindWebhook, got.events[0].Kind)
	assert.Equal(t, "⇐ push", got.events[0].Lane)
	assert.Equal(t, uint64(1), got.maxID)
	assert.NotEmpty(t, got.retentionStart)
	assert.NotEmpty(t, got.now)
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

	fetch := func(target string) decodedTimeline {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := do(t, s.router, req)
		require.Equal(t, http.StatusOK, w.Code)
		return decodeTimelineV1(t, w.Body.Bytes())
	}

	full := fetch("/api/timeline")
	require.Len(t, full.events, 2)
	require.Equal(t, uint64(2), full.maxID)

	// Cursor past the first event: only the second comes back.
	page := fetch("/api/timeline?since=1")
	require.Len(t, page.events, 1)
	assert.Equal(t, uint64(2), page.events[0].ID)

	// Cursor at the frontier: empty page, MaxID still reported.
	page = fetch("/api/timeline?since=2")
	assert.Empty(t, page.events)
	assert.Equal(t, uint64(2), page.maxID)

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

	// An unverified delivery is recorded too — on the FIXED unverified lane
	// (never a lane from its untrusted headers), claimed type as detail.
	bad := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	bad.Header.Set("X-GitHub-Event", "pull_request")
	bad.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w = do(t, s.router, bad)
	assert.Equal(t, http.StatusForbidden, w.Code)
	unverified := eventsWhere(s.timeline.Snapshot(0), "unverified")
	require.Len(t, unverified, 1)
	assert.Equal(t, "⇐ (unverified)", unverified[0].Lane)
	assert.Equal(t, "claimed event: pull_request", unverified[0].Detail)
	assert.Empty(t, unverified[0].EventType, "untrusted type must not populate trusted fields")
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

// TestTimeline_EveryExchangeRecorded: one cached-route miss puts EVERY real
// exchange on the chart — requireAuth's own /user resolution (the ghclient
// transport observer), the reveal probe, the mirror→GitHub upstream leg, and
// the inbound request itself (end-to-end). The follow-up HIT is recorded too:
// a served request is never concealed just because no upstream call happened.
func TestTimeline_EveryExchangeRecorded(t *testing.T) {
	u := newRespCacheUpstream()
	s := newFullTestStack(t, testAuth(), u.handler())

	w := do(t, s.router, authedReq(http.MethodGet, "/repos/org1/repo1/contents/.github/cfg.jsonc", nil))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "miss", w.Header().Get("X-GSM-Cache"))

	snap := s.timeline.Snapshot(0)

	// requireAuth resolved the bearer via ghclient GET /user — an "internal"
	// exchange, labeled by credential shape (no principal in ctx yet).
	internal := eventsWhere(snap, "internal")
	require.Len(t, internal, 1)
	assert.Equal(t, "/user", internal[0].Route)
	assert.True(t, strings.HasPrefix(internal[0].Actor, "token:"), "actor %q", internal[0].Actor)

	// The reveal probe (GET /repos/{owner}/{repo}) is its own exchange.
	probes := eventsWhere(snap, "probe")
	require.Len(t, probes, 1)
	assert.Equal(t, "/repos/{owner}/{repo}", probes[0].Route)
	assert.Equal(t, testUserActor, probes[0].Actor)

	// The mirror→GitHub leg of the miss.
	upstream := eventsWhere(snap, "upstream")
	require.Len(t, upstream, 1)
	assert.Equal(t, "/repos/{owner}/{repo}/contents/{path}", upstream[0].Route)
	assert.Equal(t, http.StatusOK, upstream[0].Status)

	// The inbound request, end-to-end, disposition miss.
	miss := eventsWhere(snap, DispMiss)
	require.Len(t, miss, 1)
	assert.Equal(t, "/repos/{owner}/{repo}/contents/{path}", miss[0].Route)
	assert.Equal(t, testUserActor, miss[0].Actor, "cached routes run inside requireAuth, so the principal labels the event")

	// A HIT is served from memory — still a served request, still charted.
	w = do(t, s.router, authedReq(http.MethodGet, "/repos/org1/repo1/contents/.github/cfg.jsonc", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "hit", w.Header().Get("X-GSM-Cache"))
	hits := eventsWhere(s.timeline.Snapshot(0), DispHit)
	require.Len(t, hits, 1)
	assert.Equal(t, "/repos/{owner}/{repo}/contents/{path}", hits[0].Route)
	assert.GreaterOrEqual(t, hits[0].DurMs, int64(0))
	// No second probe/upstream fetch happened (grant + cache served it).
	assert.Len(t, eventsWhere(s.timeline.Snapshot(0), "upstream"), 1)
}

// TestTimeline_OAuthRelayRecorded: the github.com login relay's upstream call
// is timed onto the chart under the mirror's fixed relay lane, anonymous
// actor.
func TestTimeline_OAuthRelayRecorded(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_x"}`))
	}))
	defer relay.Close()
	oldURL := githubOAuthTokenURL
	githubOAuthTokenURL = relay.URL
	defer func() { githubOAuthTokenURL = oldURL }()

	s := newFullTestStack(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))

	req := httptest.NewRequest(http.MethodPost, "/login/oauth/access_token", strings.NewReader("client_id=x&code=y"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := do(t, s.router, req)
	require.Equal(t, http.StatusOK, w.Code)

	relays := eventsWhere(s.timeline.Snapshot(0), "relay")
	require.Len(t, relays, 1)
	e := relays[0]
	assert.Equal(t, "POST /login/oauth/access_token", e.Lane)
	assert.Equal(t, http.StatusOK, e.Status)
	assert.Equal(t, "anonymous", e.Actor)
	assert.GreaterOrEqual(t, e.DurMs, int64(0))
}

// ---- wire format + compression (timelinewire.go, compress.go) ----

// TestTimeline_ColumnarNegotiated: a client asking for the columnar media
// type gets it — and it decodes to exactly what the JSON path reports.
func TestTimeline_AlwaysColumnar(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))
	s.timeline.RecordWebhook(time.Now(), 3*time.Millisecond, "push", "", "d-1", "o/r", "applied")
	s.timeline.RecordRequest(time.Now(), 12*time.Millisecond, "GET",
		"/repos/{owner}/{repo}/pulls", 200, DispHit, "user:1", "PazerOP")

	req := httptest.NewRequest(http.MethodGet, "/api/timeline", nil)
	req.Header.Set("Accept", timelineWireType)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := do(t, s.router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, timelineWireType, w.Header().Get("Content-Type"))

	got := decodeTimelineV1(t, w.Body.Bytes())
	require.Len(t, got.events, 2)
	assert.Equal(t, "⇐ push", got.events[0].Lane)
	assert.Equal(t, "applied", got.events[0].Disposition)
	assert.Equal(t, "GET /repos/{owner}/{repo}/pulls", got.events[1].Lane)
	assert.Equal(t, 200, got.events[1].Status)
	assert.Equal(t, "PazerOP", got.events[1].ActorName)
	assert.Equal(t, uint64(2), got.maxID)
}

// TestTimeline_NoSecondFormat: the endpoint speaks ONE encoding. Whatever a
// caller puts in Accept — nothing, */*, application/json, a future version —
// the answer is the columnar payload. The JSON alternative this replaces had
// no consumer (the chart asks for columnar; the preview replaces the fetcher
// and never calls the endpoint) and was a silent downgrade path: an Accept
// that drifted took the ~10x-slower decode with nothing failing.
func TestTimeline_NoSecondFormat(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))
	s.timeline.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d-1", "o/r", "applied")

	for _, accept := range []string{"", "*/*", "application/json", "text/html,*/*;q=0.8", "application/vnd.gsm.timeline.v2"} {
		req := httptest.NewRequest(http.MethodGet, "/api/timeline", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := do(t, s.router, req)
		require.Equal(t, http.StatusOK, w.Code, accept)
		assert.Equal(t, timelineWireType, w.Header().Get("Content-Type"), accept)
		got := decodeTimelineV1(t, w.Body.Bytes())
		require.Len(t, got.events, 1, accept)
	}
}

// TestTimeline_GzipWhenAccepted: the origin is grey-clouded, so the app is
// what compresses. A payload past the floor comes back gzipped for a client
// that accepts it, byte-identical to the uncompressed answer once inflated —
// and a client that does not accept gzip still gets plain bytes.
func TestTimeline_GzipWhenAccepted(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 2000; i++ {
		s.timeline.RecordRequest(base.Add(time.Duration(i)*time.Second), 12*time.Millisecond, "GET",
			"/repos/{owner}/{repo}/pulls", 200, DispHit, "user:1", "PazerOP")
	}

	get := func(accept, encoding string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/timeline", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if encoding != "" {
			req.Header.Set("Accept-Encoding", encoding)
		}
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := do(t, s.router, req)
		require.Equal(t, http.StatusOK, w.Code)
		return w
	}

	plain := get(timelineWireType, "")
	assert.Empty(t, plain.Header().Get("Content-Encoding"))

	zipped := get(timelineWireType, "gzip")
	require.Equal(t, "gzip", zipped.Header().Get("Content-Encoding"))
	assert.Contains(t, zipped.Header().Values("Vary"), "Accept-Encoding")
	zr, err := gzip.NewReader(bytes.NewReader(zipped.Body.Bytes()))
	require.NoError(t, err)
	inflated, err := io.ReadAll(zr)
	require.NoError(t, err)
	// Not a byte comparison: each response stamps its own `now`, so the two
	// payloads legitimately differ in the preamble. What must be identical is
	// the events they carry.
	assert.Equal(t, decodeTimelineV1(t, plain.Body.Bytes()).events,
		decodeTimelineV1(t, inflated).events, "gzip must not change the payload")
	assert.Less(t, zipped.Body.Len(), plain.Body.Len()/2, "2000 uniform events must compress hard")

	// The point of the whole exercise, asserted end to end.
	jsonPlain := get("application/json", "")
	t.Logf("2000 events: json %d B, json+gzip %d B, columnar %d B, columnar+gzip %d B",
		jsonPlain.Body.Len(), get("application/json", "gzip").Body.Len(),
		plain.Body.Len(), zipped.Body.Len())
	assert.Less(t, zipped.Body.Len(), jsonPlain.Body.Len()/10,
		"columnar+gzip must be an order of magnitude under raw JSON")

	// An explicit refusal is honored, not overridden.
	assert.Empty(t, get(timelineWireType, "gzip;q=0").Header().Get("Content-Encoding"))
}

// A payload under the floor is not worth compressing (gzip framing can make a
// tiny body bigger).
func TestTimeline_NoGzipForTinyPayloads(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))
	s.timeline.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d-1", "o/r", "applied")
	req := httptest.NewRequest(http.MethodGet, "/api/timeline", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := do(t, s.router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Content-Encoding"))
}

// TestTimeline_WindowedRead: ?from/?to is the chart's async-history read — the
// hour it paints, not the day it retains. It must return exactly the
// overlapping events, keep reporting the LIVE cursor (history never advances
// it), and reject a shape that would silently answer the wrong question.
func TestTimeline_WindowedRead(t *testing.T) {
	svc := configuredAuth(t)
	s := newFullTestStack(t, svc, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
	}))
	now := time.Now().UTC()
	for i := 6; i >= 1; i-- { // one delivery per hour, back six hours
		s.timeline.RecordWebhook(now.Add(-time.Duration(i)*time.Hour), time.Millisecond,
			"push", "", "d-"+strconv.Itoa(i), "o/r-"+strconv.Itoa(i), "applied")
	}

	get := func(target string) (*httptest.ResponseRecorder, decodedTimeline) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := do(t, s.router, req)
		var got decodedTimeline
		if w.Code == http.StatusOK {
			got = decodeTimelineV1(t, w.Body.Bytes())
		}
		return w, got
	}

	// The last 3 hours: three of the six deliveries.
	w, got := get("/api/timeline?from=" + strconv.FormatInt(now.Add(-3*time.Hour).UnixMilli(), 10))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, got.events, 3)
	assert.Equal(t, uint64(6), got.maxID, "a windowed read still reports the live cursor")

	// A bounded window in the middle.
	w, got = get("/api/timeline?from=" + strconv.FormatInt(now.Add(-5*time.Hour).UnixMilli(), 10) +
		"&to=" + strconv.FormatInt(now.Add(-3*time.Hour).UnixMilli(), 10))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, got.events, 2)

	// Rejected shapes: a garbage bound, and mixing the two read models.
	w, _ = get("/api/timeline?from=banana")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = get("/api/timeline?since=1&from=" + strconv.FormatInt(now.UnixMilli(), 10))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
