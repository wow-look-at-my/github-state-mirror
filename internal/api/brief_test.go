package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The one property the whole capture rests on: a sampled response body is
// reduced to keys and TYPES, and no value survives into anything retained.
func TestJSONSkeletonKeepsNoValues(t *testing.T) {
	body := []byte(`{
		"total_count": 2,
		"secret_token": "ghs_SUPERSECRETVALUE",
		"login": "octocat",
		"nested": {"url": "https://api.github.com/x", "flag": true, "missing": null},
		"items": [{"id": 7, "name": "one"}, {"id": 8, "name": "two"}]
	}`)
	sk := jsonSkeleton(body)

	for _, leak := range []string{"ghs_SUPERSECRETVALUE", "octocat", "api.github.com", "true", "7", "one"} {
		require.NotContains(t, sk, leak)

	}
	for _, want := range []string{"total_count: number", "secret_token: string", "flag: bool", "missing: null", "items: [{"} {
		require.Contains(t, sk, want)

	}
	// The array's length rides along (a page-shaped answer is worth knowing)
	// but its elements' values do not.
	require.Contains(t, sk, "] × 2")

}

// A non-JSON body has no shape to model — and saying so is the answer (such a
// route cannot become a tier-2 route), not a reason to retain the bytes.
func TestJSONSkeletonRefusesNonJSON(t *testing.T) {
	sk := jsonSkeleton([]byte("diff --git a/x b/x\n+secret line\n"))
	require.Equal(t, "", sk)

}

// The passthrough proxy relays a caller's own Accept-Encoding to GitHub
// unchanged, so a captured sample is the WIRE body: a caller that requests
// gzip (most HTTP libraries do, by default) gets a gzip-encoded sample here.
// Without decoding it first, json.Unmarshal always fails on gzip bytes, the
// route's shape never records a skeleton, and — because a failed sample never
// advances lastSampleAt — wantsBody keeps asking forever: the exact "no
// response outline yet" stall the operator sees no matter how long they wait.
func TestShapeStoreDecodesGzipEncodedPassthrough(t *testing.T) {
	s := newShapeStore()
	route := "/repos/{owner}/{repo}/labels"
	plain := []byte(`[{"name":"bug","color":"d73a4a"}]`)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(plain)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	require.True(t, s.wantsBody("GET", route))
	s.observe(observation{
		Method: "GET", Route: route, Path: "/repos/o/r/labels",
		Status: 200, ContentType: "application/json", ContentEncoding: "gzip",
		Body: buf.Bytes(),
	})

	// A successful decode produced a real skeleton, so the route is no longer
	// stuck asking for a re-sample on every subsequent passthrough.
	require.False(t, s.wantsBody("GET", route))

	snap := s.snapshot()
	sh, ok := snap["GET "+route]
	require.True(t, ok)
	require.Len(t, sh.Bodies, 1)
	require.Contains(t, sh.Bodies[0].Skeleton, "name: string")
	require.Contains(t, sh.Bodies[0].Skeleton, "color: string")
}

// decodeSample must never choke on a body that is not actually gzip (a
// Content-Encoding lie, or identity/absent): it falls back to the raw bytes
// rather than losing the sample.
func TestDecodeSamplePassesThroughNonGzip(t *testing.T) {
	plain := []byte(`{"a":1}`)
	require.Equal(t, plain, decodeSample(plain, ""))
	require.Equal(t, plain, decodeSample(plain, "identity"))
	require.Equal(t, plain, decodeSample(plain, "gzip")) // not actually gzip: falls back
}

func TestShapeStoreSamplingAndSnapshot(t *testing.T) {
	s := newShapeStore()
	route := "/repos/{owner}/{repo}/hooks"

	require.True(t, s.wantsBody("GET", route))

	s.observe(observation{
		Method: "GET", Route: route, Path: "/repos/o/r/hooks",
		QueryNames: []string{"per_page"}, Accept: "application/vnd.github+json",
		Caller: "pr-minder", Status: 200, ContentType: "application/json",
		Body: []byte(`[{"id":1,"active":true}]`),
	})
	require.False(t, s.wantsBody("GET", route))

	snap := s.snapshot()
	sh, ok := snap["GET "+route]
	require.True(t, ok)

	require.EqualValues(t, 1, sh.Seen)
	require.Len(t, sh.Bodies, 1)
	require.Equal(t, 200, sh.Bodies[0].Status)

	require.Len(t, sh.QueryNames, 1)
	require.Equal(t, "per_page", sh.QueryNames[0].Name)

	require.Len(t, sh.Callers, 1)
	require.Equal(t, "pr-minder", sh.Callers[0].Name)

}

// Sampling must never be the reason a body is retained whole: a body past the
// cap yields no skeleton rather than a truncated (and unparseable) one.
func TestShapeStoreIgnoresUnparseableBody(t *testing.T) {
	s := newShapeStore()
	s.observe(observation{Method: "GET", Route: "/x", Path: "/x", Status: 200, Body: []byte(`{"a":`)})
	sh := s.snapshot()["GET /x"]
	require.Empty(t, sh.Bodies, "an unparseable body must yield no skeleton")

	require.Equal(t, int64(1), sh.Seen)

}

func TestBuildBriefRanksPassthroughAndSkipsCachedRoutes(t *testing.T) {
	snap := requestLogSnapshot{
		Total:         100,
		ByDisposition: map[string]int64{DispPassthrough: 30},
		Groups: []requestGroupSnapshot{
			{Key: "GET /a", Route: "/a", Total: 50, Hit: 50},         // fully cached: not a candidate
			{Key: "GET /b", Route: "/b", Total: 20, Passthrough: 20}, // biggest gap
			{Key: "GET /c", Route: "/c", Total: 30, Passthrough: 10, Hit: 20},
			{Key: "POST /d", Route: "/d", Total: 5, Write: 5}, // a write is not a caching gap
		},
	}
	shapes := map[string]routeShapeSnapshot{
		"GET /b": {Key: "GET /b", Seen: 20, Bodies: []bodySample{{Status: 200, Skeleton: "{\n  id: number\n}"}}},
	}
	cands := buildBrief(snap, shapes, 10)
	require.Len(t, cands, 2, "only the passthrough routes are candidates")

	require.Equal(t, "GET /b", cands[0].Key, "ranked by passthrough volume")
	require.Equal(t, "GET /c", cands[1].Key)

	require.NotNil(t, cands[0].Shape, "the captured shape must be joined on")
	require.Nil(t, cands[1].Shape, "a route with no capture yet joins to nothing")

	md := renderBrief(snap, cands, "2026-08-01T00:00:00Z")
	for _, want := range []string{"GET /b", "id: number", "tier-2 contract", "assertNoURLKeys", "schema.sql"} {
		require.Contains(t, md, want)

	}
	require.NotContains(t, md, "POST /d")

}

// Nil-receiver safety: a router built without a shape store (as several tests
// do) must not panic on the recording path.
func TestShapeStoreNilSafe(t *testing.T) {
	var s *shapeStore
	require.False(t, s.wantsBody("GET", "/x"))

	s.observe(observation{Method: "GET", Route: "/x"})
	require.Empty(t, s.snapshot())

}

// The brief endpoint end to end: admin-only, and its payload carries both the
// Markdown deliverable and the structured candidates — built from traffic the
// router itself recorded, so the wiring (proxy sampling -> shape store ->
// brief) is exercised rather than mocked.
func TestDashboardBrief_AdminOnlyAndRendersRecordedTraffic(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	// A read the mirror does not cache: forwarded, recorded, and sampled.
	do(t, router, authedReq("GET", "/repos/org1/repo1/hooks?per_page=100", nil))

	// Signed out and non-admin callers get nothing.
	w := do(t, router, httptest.NewRequest("GET", "/api/brief", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	req := httptest.NewRequest("GET", "/api/brief", nil)
	req.AddCookie(mintSession(t, svc, "not-an-admin"))
	require.Equal(t, http.StatusForbidden, do(t, router, req).Code)

	req = httptest.NewRequest("GET", "/api/brief", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)

	var payload briefPayload
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.NotEmpty(t, payload.Candidates, "the forwarded read must appear as a candidate")
	require.Contains(t, payload.Markdown, "/repos/{owner}/{repo}/hooks")
	require.Contains(t, payload.Markdown, "per_page", "the query shape must be captured, by name")
	require.Contains(t, payload.Markdown, "tier-2 contract", "the checklist travels with the data")

	// A bad limit is a 400, like every other paged admin view.
	req = httptest.NewRequest("GET", "/api/brief?limit=zero", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	require.Equal(t, http.StatusBadRequest, do(t, router, req).Code)
}

// The template is parsed at init (template.Must), so a syntax error is already
// a startup panic. What a test still has to cover is EXECUTION: a field
// renamed out from under the template, or a FuncMap entry dropped, fails only
// when something renders. Both edges do.
func TestRenderBriefTemplateExecutes(t *testing.T) {
	empty := renderBrief(requestLogSnapshot{ByDisposition: map[string]int64{}}, nil, "2026-08-01T00:00:00Z")
	require.Contains(t, empty, "No route has forwarded a read uncached since restart.")
	require.Contains(t, empty, "## How to model one of these")
	require.NotContains(t, empty, "template failed to render")

	// Every optional section present at once: reasons, other dispositions,
	// debounce, query shape, sample, and a captured body.
	snap := requestLogSnapshot{
		Total:         100,
		ByDisposition: map[string]int64{DispHit: 40, DispPassthrough: 60},
		Groups: []requestGroupSnapshot{{
			Key: "GET /repos/{owner}/{repo}/hooks", Route: "/repos/{owner}/{repo}/hooks",
			Total: 60, Passthrough: 60, Hit: 0, Miss: 0, Error: 0,
			ByReason: map[string]int64{"unrouted": 60}, PassQuery: "per_page",
			Debounced: 60, UpstreamSaved: 0, Sample: "/repos/o/r/hooks",
		}},
	}
	shapes := map[string]routeShapeSnapshot{"GET /repos/{owner}/{repo}/hooks": {
		Key:         "GET /repos/{owner}/{repo}/hooks",
		QueryNames:  []countedName{{Name: "per_page", Count: 60}},
		Accepts:     []countedName{{Name: "application/vnd.github+json", Count: 60}},
		Callers:     []countedName{{Name: "pr-minder", Count: 60}},
		Statuses:    []countedInt{{Value: 200, Count: 60}},
		SamplePaths: []string{"/repos/o/r/hooks", "/repos/o/other/hooks"},
		Bodies:      []bodySample{{Status: 200, ContentType: "application/json", Bytes: 42, Skeleton: "[{\n  id: number\n}] × 1"}},
	}}
	md := renderBrief(snap, buildBrief(snap, shapes, 10), "2026-08-01T00:00:00Z")
	require.NotContains(t, md, "template failed to render")
	for _, want := range []string{
		"### 1. `GET /repos/{owner}/{repo}/hooks`",
		"100.0% of this route",
		"**Why uncached**: `unrouted` ×60",
		"all cost, no benefit", // held 60, saved 0
		"Sampled passthrough query (names only): `?per_page`",
		"Callers: `pr-minder` ×60",
		"Upstream statuses: 200 ×60",
		"More sample paths: `/repos/o/other/hooks`",
		"**Response shape — HTTP 200** (application/json, 42 bytes",
		"id: number",
	} {
		require.Contains(t, md, want)
	}
	// The fenced skeleton must be separated from the checklist heading, or the
	// two run together into one unreadable block.
	require.Contains(t, md, "```\n\n## How to model one of these")
}

// The end-to-end property the whole feature exists for: a route the mirror
// does not model at all is forwarded, its ANSWER's shape is captured, and that
// shape lands in the Markdown the button copies -- so "we have no evidence for
// this route" fixes itself the moment the route sees traffic.
//
// GET /repos/{owner}/{repo}/milestones/{number} is the live example: nothing
// claims it, so it reaches chi's NotFound, the tagged proxy, and the recorder.
func TestBrief_CapturesUnroutedRouteAndItsResponseShape(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
			return
		}
		// GitHub's milestone object, URL fields and all.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{
			"id": 208045946, "node_id": "MDU6TGFiZWwyMDgwNDU5NDY=",
			"url": "https://api.github.com/repos/o/r/milestones/3",
			"name": "bug", "color": "f29513", "default": true,
			"description": "Something isn't working"
		}`))
	})
	svc := configuredAuth(t)
	router, _, _, _ := newTestStackWithGitHub(t, svc, gh)

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/milestones/3", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Header().Get(cacheHeader), "the route is unrouted: it must be forwarded")

	req := httptest.NewRequest("GET", "/api/brief", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	var payload briefPayload
	require.NoError(t, json.Unmarshal(do(t, router, req).Body.Bytes(), &payload))

	md := payload.Markdown
	require.Contains(t, md, "GET /repos/{owner}/{repo}/milestones/{number}", "the route shape must be a candidate")
	require.Contains(t, md, "`unrouted`", "with the reason that says nothing claims this path")
	require.Contains(t, md, "**Response shape — HTTP 200**")
	// Keys and types, recursively — everything a trimmed rebuild needs to be
	// designed, and no value from the answer itself.
	for _, want := range []string{"id: number", "name: string", "default: bool", "description: string", "url: string"} {
		require.Contains(t, md, want)
	}
	for _, leak := range []string{"208045946", "f29513", "Something isn't working", "MDU6TGFiZWwyMDgwNDU5NDY="} {
		require.NotContains(t, md, leak, "a captured value must never reach the brief")
	}
}
