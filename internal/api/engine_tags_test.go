package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimirror "github.com/wow-look-at-my/api-mirror/engine"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// The tags listing, served by the embedded engine through the real router.

const tagsTarget = "/repos/org1/repo1/tags"

const tagsRoute = "/repos/{owner}/{repo}/tags"

func tagObject(name, sha string) map[string]any {
	return map[string]any{
		"name":        name,
		"zipball_url": "https://api.github.com/repos/org1/repo1/zipball/refs/tags/" + name,
		"tarball_url": "https://api.github.com/repos/org1/repo1/tarball/refs/tags/" + name,
		"commit":      map[string]any{"sha": sha, "url": "https://api.github.com/repos/org1/repo1/commits/" + sha},
		"node_id":     "REF_" + name,
	}
}

// tagsFixture is the router with the engine mounted, over a fake GitHub that counts tags calls.
type tagsFixture struct {
	router   http.Handler
	timeline *reqtimeline.Recorder
	up       *respCacheUpstream
	calls    atomic.Int32
	status   atomic.Int32
}

func newTagsFixture(t *testing.T) *tagsFixture {
	t.Helper()
	f := &tagsFixture{up: newRespCacheUpstream()}
	f.status.Store(http.StatusOK)
	f.up.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/tags") {
			return false
		}
		f.calls.Add(1)
		switch status := int(f.status.Load()); status {
		case http.StatusOK:
			writeGitHubJSON(w, []any{tagObject("v1", shaBase), tagObject("v0", shaTree1)})
		case http.StatusUnauthorized:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("Bad credentials"))
		default:
			w.WriteHeader(status)
			writeGitHubJSON(w, map[string]any{"message": "Not Found", "status": "404"})
		}
		return true
	}
	s := newFullTestStack(t, testAuth(), f.up.handler())
	f.router, f.timeline = s.router, s.timeline
	return f
}

func (f *tagsFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.router, authedReq(http.MethodGet, target, nil))
}

// tagsDisposition is the disposition of the newest inbound tags event on the Timeline.
func (f *tagsFixture) tagsDisposition(t *testing.T) string {
	t.Helper()
	events := f.timeline.Snapshot(0).Events
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Route == tagsRoute && (e.Disposition == DispHit || e.Disposition == DispMiss || e.Disposition == DispPassthrough) {
			return e.Disposition
		}
	}
	require.Fail(t, "no tags request on the Timeline")
	return ""
}

func TestEngineTags_MissThenHit(t *testing.T) {
	f := newTagsFixture(t)

	w1 := f.get(t, tagsTarget+"?per_page=100&page=1")
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString([]any{
		map[string]any{"name": "v1", "commit": map[string]any{"sha": shaBase}, "node_id": "REF_v1"},
		map[string]any{"name": "v0", "commit": map[string]any{"sha": shaTree1}, "node_id": "REF_v0"},
	}), w1.Body.String())
	assert.Equal(t, DispMiss, f.tagsDisposition(t))

	// The same page under a reordered query is the same row.
	w2 := f.get(t, tagsTarget+"?page=1&per_page=100")
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.EqualValues(t, 1, f.calls.Load())
	assert.Equal(t, DispHit, f.tagsDisposition(t))

	// Another page is its own row.
	assert.Equal(t, "miss", f.get(t, tagsTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 2, f.calls.Load())
}

func TestEngineTags_OutboundCallsAreOnTheTimeline(t *testing.T) {
	f := newTagsFixture(t)
	require.Equal(t, http.StatusOK, f.get(t, tagsTarget).Code)
	var fetch, probe bool
	for _, e := range f.timeline.Snapshot(0).Events {
		fetch = fetch || (e.Route == tagsRoute && e.Disposition == dispUpstream)
		probe = probe || (e.Route == "/repos/{owner}/{repo}" && e.Disposition == dispProbe)
	}
	assert.True(t, fetch, "the engine's fetch is charted")
	assert.True(t, probe, "the engine's reveal probe is charted")
}

func TestEngineTags_PrivateRepoNotRevealed(t *testing.T) {
	f := newTagsFixture(t)
	f.up.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeGitHubJSON(w, map[string]any{"message": "Not Found"})
	}
	for range 2 {
		w := f.get(t, tagsTarget)
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.NotContains(t, w.Body.String(), "REF_v1")
	}
	assert.Zero(t, f.calls.Load(), "a refused caller never reaches the listing")
}

func TestEngineTags_NotFoundIsStoredUnauthorizedIsNot(t *testing.T) {
	f := newTagsFixture(t)
	f.status.Store(http.StatusNotFound)
	for i, want := range []string{"miss", "hit"} {
		w := f.get(t, tagsTarget)
		require.Equal(t, http.StatusNotFound, w.Code, "request %d", i)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.EqualValues(t, 1, f.calls.Load())

	f.status.Store(http.StatusUnauthorized)
	for i := int32(2); i <= 3; i++ {
		w := f.get(t, tagsTarget+"?page=2")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.NotEqual(t, "hit", w.Header().Get(cacheHeader))
		assert.Equal(t, i, f.calls.Load())
	}
}

func TestEngineTags_UnmodeledShapesPassThrough(t *testing.T) {
	f := newTagsFixture(t)
	targets := []string{
		tagsTarget + "?per_page=101", tagsTarget + "?page=11", tagsTarget + "?sort=name",
		tagsTarget + "?page=1&page=2",
	}
	for _, target := range targets {
		w := f.get(t, target)
		assert.Equal(t, "passthrough", w.Header().Get(cacheHeader), target)
		assert.Equal(t, DispPassthrough, f.tagsDisposition(t), target)
	}
	req := authedReq(http.MethodGet, tagsTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Equal(t, "passthrough", do(t, f.router, req).Header().Get(cacheHeader))
	assert.EqualValues(t, len(targets)+1, f.calls.Load())
}

func TestEngineTags_EnterpriseSpellingReachesTheRoute(t *testing.T) {
	f := newTagsFixture(t)
	assert.Equal(t, "miss", f.get(t, "/api/v3"+tagsTarget).Header().Get(cacheHeader))
	assert.Equal(t, "hit", f.get(t, tagsTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestEngineTags_TokenlessIsThisServicesUnauthorized(t *testing.T) {
	f := newTagsFixture(t)
	w := do(t, f.router, httptest.NewRequest(http.MethodGet, tagsTarget, nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "missing Authorization header")
	assert.Zero(t, f.calls.Load())
}

// tagsRepo is the repository object of a delivery, with the clocks the engine orders by.
func tagsRepo(clock int) map[string]any {
	return map[string]any{
		"name": "repo1", "full_name": "org1/repo1",
		"owner":          map[string]any{"login": "org1"},
		"default_branch": "main",
		"pushed_at":      1767225600 + clock,
		"updated_at":     "2026-01-01T00:00:0" + string(rune('0'+clock)) + "Z",
	}
}

// A delivery the engine reaches through this service's own webhook path drops the listing.
func TestEngineTags_DeliveriesRefetch(t *testing.T) {
	f := newTagsFixture(t)
	deliveries := []struct {
		event   string
		payload map[string]any
	}{
		{"push", map[string]any{"ref": "refs/tags/v1", "before": shaBase, "after": shaTip, "repository": tagsRepo(1), "commits": []any{}}},
		{"create", map[string]any{"ref": "v2", "ref_type": "tag", "repository": tagsRepo(2)}},
		{"delete", map[string]any{"ref": "v0", "ref_type": "tag", "repository": tagsRepo(3)}},
		{"repository", map[string]any{"action": "renamed", "repository": tagsRepo(4)}},
	}
	for i, d := range deliveries {
		require.Equal(t, "miss", f.get(t, tagsTarget).Header().Get(cacheHeader), "before delivery %d", i)
		require.Equal(t, "hit", f.get(t, tagsTarget).Header().Get(cacheHeader), "before delivery %d", i)
		postWebhookJSON(t, f.router, d.event, d.payload)
	}
	assert.Equal(t, "miss", f.get(t, tagsTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, len(deliveries)+1, f.calls.Load())
}

func TestEngineTags_BranchPushKeepsTheListing(t *testing.T) {
	f := newTagsFixture(t)
	require.Equal(t, "miss", f.get(t, tagsTarget).Header().Get(cacheHeader))
	postWebhookJSON(t, f.router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip, "repository": tagsRepo(1), "commits": []any{},
	})
	assert.Equal(t, "hit", f.get(t, tagsTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 1, f.calls.Load())
}

// countingEngine records what reaches it.
type countingEngine struct{ served atomic.Int32 }

func (c *countingEngine) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.served.Add(1)
	w.WriteHeader(http.StatusTeapot)
}

func (c *countingEngine) Declares(method, path string) bool {
	return method == http.MethodGet && path == tagsTarget
}

func (c *countingEngine) SetRequestSink(func(apimirror.Request)) {}

func TestEngineDispatch_UndeclaredNeverReachesTheEngine(t *testing.T) {
	eng := &countingEngine{}
	passthrough := func(next http.Handler) http.Handler { return next }
	var rest atomic.Int32
	h := engineDispatch(eng, passthrough)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rest.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, tagsTarget, nil),
		httptest.NewRequest(http.MethodGet, "/repos/org1/repo1/branches", nil),
		httptest.NewRequest(http.MethodPost, "/_engine/webhook", nil),
		httptest.NewRequest(http.MethodGet, "/_mirror/", nil),
	} {
		assert.Equal(t, http.StatusNoContent, do(t, h, r).Code, r.Method+" "+r.URL.Path)
	}
	assert.Zero(t, eng.served.Load())
	assert.Equal(t, http.StatusTeapot, do(t, h, httptest.NewRequest(http.MethodGet, tagsTarget, nil)).Code)
	assert.EqualValues(t, 1, eng.served.Load())
	assert.EqualValues(t, 4, rest.Load())
}

// The real router keeps the engine's own paths away from it.
func TestEngineDispatch_EnginePathsStayOnThisService(t *testing.T) {
	f := newTagsFixture(t)
	w := do(t, f.router, authedReq(http.MethodPost, "/_engine/webhook", strings.NewReader("{}")))
	assert.NotEqual(t, http.StatusAccepted, w.Code)
	assert.NotEqual(t, "invalidated", w.Header().Get("X-GSM-Disposition"))
}
