package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeRoute(t *testing.T) {
	sha := strings.Repeat("9f3c1a2b", 5) // 40 hex chars
	cases := []struct {
		path, want string
	}{
		// /repos subresources — the real shapes this system serves.
		{"/repos/wow-look-at-my/buildhost/compare/master...oci-cache", "/repos/{owner}/{repo}/compare/{basehead}"},
		{"/repos/o/r/compare/main...claude/zen-volta", "/repos/{owner}/{repo}/compare/{basehead}"}, // slashed branch
		{"/repos/o/r/commits", "/repos/{owner}/{repo}/commits"},
		{"/repos/o/r/commits/" + sha, "/repos/{owner}/{repo}/commits/{sha}"},
		{"/repos/o/r/commits/master", "/repos/{owner}/{repo}/commits/{ref}"},
		{"/repos/o/r/commits/master/status", "/repos/{owner}/{repo}/commits/{ref}/status"},
		{"/repos/o/r/commits/claude/zen-volta/status", "/repos/{owner}/{repo}/commits/{ref}/status"}, // slashed ref
		{"/repos/o/r/commits/" + sha + "/check-runs", "/repos/{owner}/{repo}/commits/{ref}/check-runs"},
		{"/repos/o/r/commits/" + sha + "/statuses", "/repos/{owner}/{repo}/commits/{ref}/statuses"},
		{"/repos/o/r/commits/" + sha + "/pulls", "/repos/{owner}/{repo}/commits/{ref}/pulls"},
		{"/repos/o/r/contents/README.md", "/repos/{owner}/{repo}/contents/{path}"},
		{"/repos/o/r/contents/.github/config/pr-minder/pr-minder.jsonc", "/repos/{owner}/{repo}/contents/{path}"},
		{"/repos/o/r/git/commits/" + sha, "/repos/{owner}/{repo}/git/commits/{sha}"},
		{"/repos/o/r/pulls", "/repos/{owner}/{repo}/pulls"},
		{"/repos/o/r/pulls/318", "/repos/{owner}/{repo}/pulls/{number}"},
		{"/repos/o/r/pulls/318/files", "/repos/{owner}/{repo}/pulls/{number}/files"},
		{"/repos/o/r/pulls/318/update-branch", "/repos/{owner}/{repo}/pulls/{number}/update-branch"},
		{"/repos/o/r/pulls/318/reviews/2/comments", "/repos/{owner}/{repo}/pulls/{number}/reviews/…"},
		{"/repos/o/r/branches", "/repos/{owner}/{repo}/branches"},
		{"/repos/o/r/branches/claude/zen-volta", "/repos/{owner}/{repo}/branches/{branch}"},
		{"/repos/o/r/labels", "/repos/{owner}/{repo}/labels"},
		{"/repos/o/r/labels/merge conflict", "/repos/{owner}/{repo}/labels/{name}"}, // decoded space
		{"/repos/o/r/statuses/" + sha, "/repos/{owner}/{repo}/statuses/{sha}"},
		{"/repos/o/r/actions/runs", "/repos/{owner}/{repo}/actions/runs"},
		{"/repos/o/r/actions/runs/8123456789", "/repos/{owner}/{repo}/actions/runs/{number}"},
		{"/repos/o/r/installation", "/repos/{owner}/{repo}/installation"},
		{"/repos/o/r/issues", "/repos/{owner}/{repo}/issues"},
		{"/repos/o/r/issues/12", "/repos/{owner}/{repo}/issues/{number}"},
		{"/repos/o/r/git/refs/heads/claude/foo", "/repos/{owner}/{repo}/git/refs/heads/…"}, // unknown deep tail
		{"/repos/o/r/merges", "/repos/{owner}/{repo}/merges"},
		{"/repos/o/r", "/repos/{owner}/{repo}"},
		{"/repos/o", "/repos/{owner}"},
		{"/repos", "/repos"},

		// Non-/repos routes.
		{"/app/installations/481/access_tokens", "/app/installations/{id}/access_tokens"},
		{"/app/installations/481", "/app/installations/{id}"},
		{"/app", "/app"},
		{"/graphql", "/graphql"},
		{"/user", "/user"},
		{"/rate_limit", "/rate_limit"},
		{"/search/issues", "/search/issues"},
		{"/orgs/wow-look-at-my/repos", "/orgs/{org}/repos"},
		{"/orgs/wow-look-at-my", "/orgs/{org}"},
		{"/users/octocat/repos", "/users/{username}/repos"},
		{"/login/oauth/access_token", "/login/oauth/…"},

		// Garbage / degenerate inputs — total function, never a panic.
		{"", "/"},
		{"/", "/"},
		{"///", "/"},
		{"no-leading-slash", "/no-leading-slash"},
		{"/a//b", "/a/b"},
		{"/a/b/c/d/e", "/a/b/…"},
		{"/12345", "/{number}"},
		{"/" + sha + "/x/y", "/{sha}/x/…"},
		{"/répos/über/straße/x", "/répos/über/…"}, // unicode survives
	}
	for _, c := range cases {
		assert.Equal(t, c.want, normalizeRoute(c.path), "path %q", c.path)
	}

	// A pathological giant segment is clamped, rune-safely.
	long := normalizeRoute("/x/" + strings.Repeat("ä", 500))
	assert.LessOrEqual(t, len(long), routeMaxLen+len("…"))
	assert.True(t, strings.HasSuffix(long, "…"), "clamped route ends in ellipsis: %q", long)
}

// TestRequestLog_Groups verifies group accumulation: per-disposition splits,
// totals, the raw-path sample, and that different concrete paths of one shape
// land in one group.
func TestRequestLog_Groups(t *testing.T) {
	l := newRequestLog()
	l.record(callerIdent{Key: "a"}, "GET", "/repos/o/r/compare/a...b", DispHit)
	l.record(callerIdent{Key: "a"}, "GET", "/repos/o/r2/compare/x...y", DispMiss)
	l.recordStatus(callerIdent{Key: "a"}, "GET", "/repos/o/r3/compare/p...q", DispPassthrough, 200)
	l.record(callerIdent{Key: "a"}, "POST", "/graphql", DispHit)
	l.recordStatus(callerIdent{Key: "a"}, "PATCH", "/repos/o/r/pulls/9", DispWrite, 200)
	l.record(callerIdent{Key: "a"}, "GET", "/repos/o/r/pulls/9", DispError)

	snap := l.snapshot(10)
	require.Len(t, snap.Groups, 4)

	byKey := map[string]requestGroupSnapshot{}
	for _, g := range snap.Groups {
		byKey[g.Key] = g
	}

	cmp := byKey["GET /repos/{owner}/{repo}/compare/{basehead}"]
	assert.Equal(t, int64(3), cmp.Total, "three compare paths share one group")
	assert.Equal(t, int64(1), cmp.Hit)
	assert.Equal(t, int64(1), cmp.Miss)
	assert.Equal(t, int64(1), cmp.Passthrough)
	assert.Equal(t, int64(0), cmp.Write)
	assert.Equal(t, "GET", cmp.Method)
	assert.Equal(t, "/repos/{owner}/{repo}/compare/{basehead}", cmp.Route)
	assert.Equal(t, "/repos/o/r3/compare/p...q", cmp.Sample, "sample is the most recent raw path")
	ls, err := time.Parse(time.RFC3339, cmp.LastSeen)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), ls, time.Minute)

	// Same route, different method -> different groups.
	assert.Equal(t, int64(1), byKey["PATCH /repos/{owner}/{repo}/pulls/{number}"].Write)
	assert.Equal(t, int64(1), byKey["GET /repos/{owner}/{repo}/pulls/{number}"].Error)
	assert.Equal(t, int64(1), byKey["POST /graphql"].Hit)

	// Groups are sorted by total desc; the compare group (3) leads.
	assert.Equal(t, "GET /repos/{owner}/{repo}/compare/{basehead}", snap.Groups[0].Key)
}

// TestRequestLog_GroupsBound verifies the group map is bounded: once full, new
// shapes are dropped while existing groups keep counting.
func TestRequestLog_GroupsBound(t *testing.T) {
	l := newRequestLog()
	for i := 0; i < requestGroupsCap+50; i++ {
		// Two-segment literal paths each normalize to a distinct shape.
		l.record(callerIdent{Key: "a"}, "GET", fmt.Sprintf("/bucket%d/leaf", i), DispPassthrough)
	}
	l.mu.Lock()
	assert.Len(t, l.groups, requestGroupsCap, "group map is capped")
	l.mu.Unlock()

	// An existing group still counts after the cap is hit.
	l.record(callerIdent{Key: "a"}, "GET", "/bucket0/leaf", DispPassthrough)
	l.mu.Lock()
	g := l.groups["GET /bucket0/leaf"]
	l.mu.Unlock()
	require.NotNil(t, g)
	assert.Equal(t, int64(2), g.total)

	// Totals still count every request, dropped shapes included.
	assert.Equal(t, int64(requestGroupsCap+51), l.snapshot(0).Total)
}

// TestRequestLog_GroupSnapshotSortedAndCapped verifies snapshot ordering
// (total desc, key asc on ties) and the payload cap.
func TestRequestLog_GroupSnapshotSortedAndCapped(t *testing.T) {
	l := newRequestLog()
	for i := 0; i < requestGroupsSnapshotCap+20; i++ {
		path := fmt.Sprintf("/tie%03d/leaf", i)
		l.record(callerIdent{Key: "a"}, "GET", path, DispHit) // 120 groups with total 1 each
	}
	// One hot group that must sort first.
	for i := 0; i < 5; i++ {
		l.record(callerIdent{Key: "a"}, "POST", "/graphql", DispMiss)
	}

	snap := l.snapshot(0)
	require.Len(t, snap.Groups, requestGroupsSnapshotCap, "payload is capped")
	assert.Equal(t, "POST /graphql", snap.Groups[0].Key, "hottest group first")
	assert.Equal(t, int64(5), snap.Groups[0].Total)
	// Equal-total groups are key-ordered (deterministic payloads).
	assert.Equal(t, "GET /tie000/leaf", snap.Groups[1].Key)
	assert.Equal(t, "GET /tie001/leaf", snap.Groups[2].Key)
}
