package api

import (
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
	for _, want := range []string{"GET /b", "id: number", "tier-2 contract", "assertNoURLKeys", "SchemaVersion"} {
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
