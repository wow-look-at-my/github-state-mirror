package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryShape: the sampled query shape is the sorted parameter NAMES only.
// Values are deliberately never recorded -- a value can carry a credential and
// is unbounded, while the name set is both safe and exactly what the routes'
// shape guards test.
func TestQueryShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"sorted", "status=queued&per_page=100", "per_page,status"},
		{"single", "head_sha=deadbeef", "head_sha"},
		{"repeated param collapses to one name", "page=1&page=2", "page"},
		{"valueless param still names itself", "draft", "draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, tc.want, queryShape(q))
		})
	}

	// No VALUE may ever survive into the recorded shape, however sensitive.
	q, err := url.ParseQuery("token=ghp_supersecretvalue&access_token=abc123")
	require.NoError(t, err)
	shape := queryShape(q)
	assert.Equal(t, "access_token,token", shape)
	assert.NotContains(t, shape, "ghp_supersecretvalue")
	assert.NotContains(t, shape, "abc123")
}

// TestRequestLog_ReasonOnlyOnPassthrough: a reason explains an uncached
// forward and nothing else. A stray hint riding the context of a hit, miss, or
// write must never reach the tally -- a route that forwards, absorbs, then
// serves would otherwise book its success as a caching gap.
func TestRequestLog_ReasonOnlyOnPassthrough(t *testing.T) {
	l := newRequestLog()
	hinted := func(disp string) *http.Request {
		r := httptest.NewRequest("GET", "/repos/o/r/actions/runs?status=queued", nil)
		return r.WithContext(withPassthroughReason(r.Context(), PassQuery))
	}
	l.observeStatus(hinted(DispHit), DispHit, 0)
	l.observeStatus(hinted(DispMiss), DispMiss, 200)
	l.observeStatus(hinted(DispWrite), DispWrite, 201)
	l.observeStatus(hinted(DispPassthrough), DispPassthrough, 200)

	snap := l.snapshot(10)
	require.Len(t, snap.Groups, 1)
	g := snap.Groups[0]
	assert.Equal(t, int64(4), g.Total)
	assert.Equal(t, map[string]int64{PassQuery: 1}, g.ByReason, "only the passthrough is explained")
	assert.Equal(t, "status", g.PassQuery)

	var reasons []string
	for _, e := range snap.Recent {
		if e.Reason != "" {
			reasons = append(reasons, e.Disposition)
		}
	}
	assert.Equal(t, []string{DispPassthrough}, reasons, "only the passthrough row carries a reason")
}

// TestRequestLog_PassQuerySamplePrefersNonEmpty: a route whose gap is a query
// filter also sees bare-path passthroughs. The bare ones must not erase the
// evidence -- the recorded sample keeps the last NON-empty shape.
func TestRequestLog_PassQuerySamplePrefersNonEmpty(t *testing.T) {
	l := newRequestLog()
	pass := func(target string) {
		r := httptest.NewRequest("GET", target, nil)
		r = r.WithContext(withPassthroughReason(r.Context(), PassQuery))
		l.observeStatus(r, DispPassthrough, 200)
	}
	pass("/repos/o/r/actions/runs?status=queued&per_page=100")
	pass("/repos/o/r/actions/runs") // bare: must not clobber the shape above

	snap := l.snapshot(10)
	require.Len(t, snap.Groups, 1)
	assert.Equal(t, "per_page,status", snap.Groups[0].PassQuery)
}

// TestPassthroughReasons_EndToEnd drives the REAL production traffic shapes
// through the router and asserts each uncached forward records WHY. Before
// this, the dashboard showed only "1140 passthrough" on a route that also
// hits 2207 times, and telling the two apart meant reading the caller's
// source -- which is how the runs route's dominant shape stayed unmodeled
// for as long as it did.
func TestPassthroughReasons_EndToEnd(t *testing.T) {
	svc := configuredAuth(t)
	u := newWorkflowRunsUpstream()
	s := newFullTestStack(t, svc, u.handler())
	router := s.router

	for _, tc := range []struct {
		name       string
		target     string
		accept     string
		wantReason string
		wantQuery  string
	}{{
		name:       "runs listing filtered by an unmodeled param",
		target:     "/repos/org1/repo1/actions/runs?event=push&per_page=100",
		wantReason: PassQuery,
		wantQuery:  "event,per_page",
	}, {
		name:       "non-default Accept",
		target:     "/repos/org1/repo1/actions/runs?head_sha=" + shaTip,
		accept:     "application/vnd.github.raw",
		wantReason: PassAccept,
		wantQuery:  "head_sha",
	}, {
		name:       "short (ambiguous) sha on the git-commit read",
		target:     "/repos/org1/repo1/git/commits/abc123",
		wantReason: PassPath,
	}, {
		// /orgs/{org}/actions/runners was the original example here; it is
		// now a cached route (respcache_orgrunners.go), and /rate_limit
		// followed it (respcache_ratelimit.go) -- genuinely unrouted paths
		// keep shrinking as routes get modeled. /meta has no cached route and
		// none is planned (see docs/cache/uncacheable-routes.md).
		name:       "no cached route claims the path",
		target:     "/meta",
		wantReason: PassUnrouted,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedReq("GET", tc.target, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			w := do(t, router, req)
			require.NotEqual(t, http.StatusInternalServerError, w.Code)
			assert.Empty(t, w.Header().Get(cacheHeader), "an unmodeled shape must pass through")

			e := lastPassthrough(t, router, svc, tc.target)
			assert.Equal(t, tc.wantReason, e.Reason, "recorded reason for %s", tc.target)

			if tc.wantQuery != "" {
				// The group's sampled shape is the most recent non-empty one,
				// so right after this case's request it is this case's.
				assert.Equal(t, tc.wantQuery, groupFor(t, router, svc, "GET /repos/{owner}/{repo}/actions/runs").PassQuery)
			}
		})
	}

	// The per-route aggregate is what the dashboard's "Top uncached requests"
	// table reads: the coordinator's poll is booked as an unmodeled QUERY, and
	// the offending parameter names are named outright.
	snap := requestsSnapshot(t, router, svc)
	byKey := map[string]requestGroupSnapshot{}
	for _, g := range snap.Groups {
		byKey[g.Key] = g
	}
	runs, ok := byKey["GET /repos/{owner}/{repo}/actions/runs"]
	require.True(t, ok, "the actions/runs group exists")
	assert.Equal(t, int64(1), runs.ByReason[PassQuery])
	assert.Equal(t, int64(1), runs.ByReason[PassAccept])

	metaGroup, ok := byKey["GET /meta"]
	require.True(t, ok, "the /meta group exists")
	assert.Equal(t, int64(1), metaGroup.ByReason[PassUnrouted])
}

// TestPassthroughReason_MethodNotAllowed: the required-builds status PUBLISH
// (POST) lands on the GET-only statuses alias and falls to MethodNotAllowed.
// It is a WRITE, not a caching gap -- so it carries the routing reason while
// staying out of the passthrough tally entirely.
func TestPassthroughReason_MethodNotAllowed(t *testing.T) {
	svc := configuredAuth(t)
	u := newWorkflowRunsUpstream()
	s := newFullTestStack(t, svc, u.handler())

	w := do(t, s.router, authedReq("POST", "/repos/org1/repo1/statuses/"+shaTip, nil))
	require.NotEqual(t, http.StatusInternalServerError, w.Code)

	snap := requestsSnapshot(t, s.router, svc)
	var found bool
	for _, e := range snap.Recent {
		if e.Method == "POST" && e.Path == "/repos/org1/repo1/statuses/"+shaTip {
			found = true
			assert.Equal(t, DispWrite, e.Disposition, "a mutation is a write, not a caching gap")
			assert.Empty(t, e.Reason, "a write has no uncached-read reason to explain")
		}
	}
	require.True(t, found, "the status publish must be in the log")
}

// groupFor reads one route-shape group out of the admin payload.
func groupFor(t *testing.T, router http.Handler, svc *auth.Service, key string) requestGroupSnapshot {
	t.Helper()
	for _, g := range requestsSnapshot(t, router, svc).Groups {
		if g.Key == key {
			return g
		}
	}
	t.Fatalf("no group %q", key)
	return requestGroupSnapshot{}
}

// requestsSnapshot reads the admin /api/requests payload.
func requestsSnapshot(t *testing.T, router http.Handler, svc *auth.Service) requestLogSnapshot {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/requests", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var snap requestLogSnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
	return snap
}

// lastPassthrough finds the most recent recorded event for a target's path.
func lastPassthrough(t *testing.T, router http.Handler, svc *auth.Service, target string) requestEvent {
	t.Helper()
	path := target
	if i := len(path); i > 0 {
		if u, err := url.Parse(target); err == nil {
			path = u.Path
		}
	}
	snap := requestsSnapshot(t, router, svc)
	for _, e := range snap.Recent { // newest first
		if e.Path == path && e.Disposition == DispPassthrough {
			return e
		}
	}
	t.Fatalf("no passthrough recorded for %s", target)
	return requestEvent{}
}
