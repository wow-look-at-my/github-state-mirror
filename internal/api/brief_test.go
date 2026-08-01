package api

import (
	"strings"
	"testing"
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
		if strings.Contains(sk, leak) {
			t.Fatalf("skeleton leaked a value %q:\n%s", leak, sk)
		}
	}
	for _, want := range []string{"total_count: number", "secret_token: string", "flag: bool", "missing: null", "items: [{"} {
		if !strings.Contains(sk, want) {
			t.Fatalf("skeleton missing %q:\n%s", want, sk)
		}
	}
	// The array's length rides along (a page-shaped answer is worth knowing)
	// but its elements' values do not.
	if !strings.Contains(sk, "] × 2") {
		t.Fatalf("skeleton lost the array length:\n%s", sk)
	}
}

// A non-JSON body has no shape to model — and saying so is the answer (such a
// route cannot become a tier-2 route), not a reason to retain the bytes.
func TestJSONSkeletonRefusesNonJSON(t *testing.T) {
	if sk := jsonSkeleton([]byte("diff --git a/x b/x\n+secret line\n")); sk != "" {
		t.Fatalf("expected no skeleton for an opaque body, got %q", sk)
	}
}

func TestShapeStoreSamplingAndSnapshot(t *testing.T) {
	s := newShapeStore()
	route := "/repos/{owner}/{repo}/hooks"

	if !s.wantsBody("GET", route) {
		t.Fatal("a never-sampled route should want a body sample")
	}
	s.observe(observation{
		Method: "GET", Route: route, Path: "/repos/o/r/hooks",
		QueryNames: []string{"per_page"}, Accept: "application/vnd.github+json",
		Caller: "pr-minder", Status: 200, ContentType: "application/json",
		Body: []byte(`[{"id":1,"active":true}]`),
	})
	if s.wantsBody("GET", route) {
		t.Fatal("a just-sampled route should not immediately re-sample")
	}

	snap := s.snapshot()
	sh, ok := snap["GET "+route]
	if !ok {
		t.Fatalf("route missing from snapshot: %v", snap)
	}
	if sh.Seen != 1 || len(sh.Bodies) != 1 || sh.Bodies[0].Status != 200 {
		t.Fatalf("unexpected snapshot: %+v", sh)
	}
	if len(sh.QueryNames) != 1 || sh.QueryNames[0].Name != "per_page" {
		t.Fatalf("query names not captured: %+v", sh.QueryNames)
	}
	if len(sh.Callers) != 1 || sh.Callers[0].Name != "pr-minder" {
		t.Fatalf("caller not captured: %+v", sh.Callers)
	}
}

// Sampling must never be the reason a body is retained whole: a body past the
// cap yields no skeleton rather than a truncated (and unparseable) one.
func TestShapeStoreIgnoresUnparseableBody(t *testing.T) {
	s := newShapeStore()
	s.observe(observation{Method: "GET", Route: "/x", Path: "/x", Status: 200, Body: []byte(`{"a":`)})
	sh := s.snapshot()["GET /x"]
	if len(sh.Bodies) != 0 {
		t.Fatalf("expected no body sample from unparseable JSON, got %+v", sh.Bodies)
	}
	if sh.Seen != 1 {
		t.Fatalf("the request itself should still be counted, got %d", sh.Seen)
	}
}

func TestBuildBriefRanksPassthroughAndSkipsCachedRoutes(t *testing.T) {
	snap := requestLogSnapshot{
		Total:         100,
		ByDisposition: map[string]int64{DispPassthrough: 30},
		Groups: []requestGroupSnapshot{
			{Key: "GET /a", Route: "/a", Total: 50, Hit: 50},              // fully cached: not a candidate
			{Key: "GET /b", Route: "/b", Total: 20, Passthrough: 20},      // biggest gap
			{Key: "GET /c", Route: "/c", Total: 30, Passthrough: 10, Hit: 20},
			{Key: "POST /d", Route: "/d", Total: 5, Write: 5},             // a write is not a caching gap
		},
	}
	shapes := map[string]routeShapeSnapshot{
		"GET /b": {Key: "GET /b", Seen: 20, Bodies: []bodySample{{Status: 200, Skeleton: "{\n  id: number\n}"}}},
	}
	cands := buildBrief(snap, shapes, 10)
	if len(cands) != 2 {
		t.Fatalf("expected only the two passthrough routes, got %d: %+v", len(cands), cands)
	}
	if cands[0].Key != "GET /b" || cands[1].Key != "GET /c" {
		t.Fatalf("candidates not ranked by passthrough volume: %+v", cands)
	}
	if cands[0].Shape == nil || cands[1].Shape != nil {
		t.Fatalf("shapes joined incorrectly: %+v", cands)
	}

	md := renderBrief(snap, cands, "2026-08-01T00:00:00Z")
	for _, want := range []string{"GET /b", "id: number", "tier-2 contract", "assertNoURLKeys", "SchemaVersion"} {
		if !strings.Contains(md, want) {
			t.Fatalf("brief markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "POST /d") {
		t.Fatalf("a write-only route must not appear as a caching candidate:\n%s", md)
	}
}

// Nil-receiver safety: a router built without a shape store (as several tests
// do) must not panic on the recording path.
func TestShapeStoreNilSafe(t *testing.T) {
	var s *shapeStore
	if s.wantsBody("GET", "/x") {
		t.Fatal("a nil store never wants a sample")
	}
	s.observe(observation{Method: "GET", Route: "/x"})
	if len(s.snapshot()) != 0 {
		t.Fatal("a nil store has no shapes")
	}
}
