package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Git-commit route tests (GET /repos/{owner}/{repo}/git/commits/{sha}); the
// shared fake upstream (respCacheUpstream, with its settable gitCommit
// answer) lives in respcache_test.go.

// TestCachedGitCommit_HitImmuneToPush: a fetched git commit is absorbed,
// rebuilt trimmed (node_id/verification/urls dropped), served from state on
// the next read — and, being immutable, survives push events untouched.
func TestCachedGitCommit_HitImmuneToPush(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/commits/" + shaCommit

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"sha": %q,
		"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-01T10:00:00Z"},
		"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-01T10:05:00Z"},
		"message": "fix: a thing <with> & symbols",
		"tree": {"sha": %q},
		"parents": [{"sha": %q}]
	}`, shaCommit, shaTree1, shaBase), w1.Body.String())

	// Push events do NOT invalidate immutable commit state.
	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits), "immutable commit must survive push events")
}

// TestCachedGitCommit_AbsorbedFromPushWebhook: a push payload's commits are
// upserted into GLOBAL truth — for a repo nobody ever fetched — so the
// post-push read hits WITHOUT any GitHub fetch ever having happened, and
// rebuilds to the same trimmed shape a fetch-sourced row does. (The reader
// still pays the reveal probe: their own proof of repo access.)
func TestCachedGitCommit_AbsorbedFromPushWebhook(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// No seeding: the dispatcher applies to GLOBAL truth unconditionally —
	// this repo has never been fetched by anyone.
	push := fmt.Sprintf(`{
		"repository": {"name":"repo1","owner":{"login":"org1"}},
		"before": %q, "after": %q, "forced": false,
		"head_commit": {"timestamp": "2026-07-03T10:00:00Z"},
		"commits": [
			{"id": %q, "tree_id": %q, "message": "first", "timestamp": "2026-07-03T09:59:00Z",
			 "author": {"name":"Alice","email":"alice@example.com"},
			 "committer": {"name":"Bob","email":"bob@example.com"}},
			{"id": %q, "tree_id": %q, "message": "second", "timestamp": "2026-07-03T10:00:00Z",
			 "author": {"name":"Alice","email":"alice@example.com"},
			 "committer": {"name":"Bob","email":"bob@example.com"}}
		]
	}`, shaBase, shaTip, shaMid, shaTree1, shaTip, shaTree2)
	postWebhook(t, router, "push", push)

	// First commit: parent is the payload's `before`.
	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaMid, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "hit", w1.Header().Get(cacheHeader), "webhook-absorbed commit must be a hit")
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"sha": %q,
		"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-03T09:59:00Z"},
		"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-03T09:59:00Z"},
		"message": "first",
		"tree": {"sha": %q},
		"parents": [{"sha": %q}]
	}`, shaMid, shaTree1, shaBase), w1.Body.String())

	// Second commit: parent is its predecessor in the chain.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaTip, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	var tip struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &tip))
	require.Len(t, tip.Parents, 1)
	assert.Equal(t, shaMid, tip.Parents[0].SHA)
	assert.Equal(t, shaTree2, tip.Tree.SHA)

	assert.Equal(t, int32(0), atomic.LoadInt32(&u.commitHits),
		"push-absorbed commits must serve without ANY GitHub fetch ever having happened")
}

// TestCachedGitCommit_ForcedPushNotAbsorbed: a forced push's payload chain is
// untrustworthy (before is not the parent), so nothing is absorbed and the
// read falls back to a normal fetch-miss.
func TestCachedGitCommit_ForcedPushNotAbsorbed(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	push := fmt.Sprintf(`{
		"repository": {"name":"repo1","owner":{"login":"org1"}},
		"before": %q, "after": %q, "forced": true,
		"commits": [
			{"id": %q, "tree_id": %q, "message": "rewritten", "timestamp": "2026-07-03T10:00:00Z",
			 "author": {"name":"A","email":"a@x"}, "committer": {"name":"B","email":"b@x"}}
		]
	}`, shaBase, shaTip, shaTip, shaTree1)
	postWebhook(t, router, "push", push)

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaTip, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "forced-push commits must not be absorbed")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits))
}

// gitCommit404Upstream makes the fake's git-commit endpoint answer GitHub's
// 404 (a GC'd or never-pushed sha).
func gitCommit404Upstream(u *respCacheUpstream) {
	u.gitCommit = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest/git/commits","status":"404"}`))
	}
}

// TestCachedGitCommit_404MissMarker: round 2's stance change -- round 1 never
// cached a git-commit 404 ("a missing sha can be pushed later"), but the
// dominant traffic is pr-minder's mergeWouldBeEmpty re-reading GC'd
// test-merge shas on every fleet sweep, each read a fresh upstream 404
// forever. The 404 is now absorbed as an EXPIRING miss marker (rebuilt in
// the contents route's notFoundJSON shape, relayed as a miss), and the
// second read is a cached 404 hit with zero upstream calls.
func TestCachedGitCommit_404MissMarker(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	gitCommit404Upstream(u)
	target := "/repos/org1/repo1/git/commits/" + shaCommit

	// Miss: the 404 is absorbed as a marker and relayed REBUILT.
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{"message":"Not Found","status":"404"}`, w1.Body.String(),
		"the 404 rebuilds in the contents route's shape, documentation_url dropped")

	// Hit: the marker answers, zero upstream calls.
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits), "a cached 404 must not call upstream")
}

// TestCachedGitCommit_404MarkerClearedByAbsorb: the un-miss invariant. A sha
// carrying a 404 marker later materializes -- here via a push webhook whose
// payload absorbs the commit (every absorb path funnels through
// ghdata.upsertGitCommit, which clears the marker) -- and the next read
// serves the 200 rebuild immediately, from the absorbed row, rather than the
// stale 404 or a refetch.
func TestCachedGitCommit_404MarkerClearedByAbsorb(t *testing.T) {
	router, _, db, u := respCacheStack(t)
	gitCommit404Upstream(u)
	target := "/repos/org1/repo1/git/commits/" + shaMid

	// Earn the marker.
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w2.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits))

	// The sha is pushed: the payload absorb upserts the commit, and the
	// upsert clears the marker.
	postWebhook(t, router, "push", fmt.Sprintf(`{
		"repository": {"name":"repo1","owner":{"login":"org1"}},
		"before": %q, "after": %q, "forced": false,
		"commits": [
			{"id": %q, "tree_id": %q, "message": "now it exists", "timestamp": "2026-07-03T10:00:00Z",
			 "author": {"name":"Alice","email":"alice@example.com"},
			 "committer": {"name":"Bob","email":"bob@example.com"}}
		]
	}`, shaBase, shaMid, shaMid, shaTree1))

	var markers int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM git_commit_miss_cache`).Scan(&markers))
	assert.Zero(t, markers, "a real commit upsert must clear the sha's 404 marker")

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code, "the materialized sha must stop answering 404 immediately")
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader), "the absorbed row serves the 200 rebuild")
	assert.Contains(t, w3.Body.String(), "now it exists")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits), "no refetch needed")

	// Belt and braces: a marker's TTL expiry alone also stops it answering.
	gitCommit404Upstream(u)
	miss := "/repos/org1/repo1/git/commits/" + shaTip
	do(t, router, authedReq("GET", miss, nil))
	_, err := db.Exec(`UPDATE git_commit_miss_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)
	w4 := do(t, router, authedReq("GET", miss, nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader), "an expired marker is a miss (refetch)")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.commitHits))
}
