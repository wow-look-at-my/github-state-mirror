package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Single-PR route tests (GET /repos/{owner}/{repo}/pulls/{number}); shared
// fixtures (upstreamPR, pullsCacheStack, ...) live in respcache_pulls_test.go.

// TestCachedPull_MergeableGate covers the single-PR flow end to end: a null
// mergeable answer is served but never gates a hit (each read refetches until
// GitHub resolves), a resolved answer is absorbed and then served from state.
func TestCachedPull_MergeableGate(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	mergeable := "null"
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		switch mergeable {
		case "true":
			pr["mergeable"] = true
		case "false":
			pr["mergeable"] = false
		default:
			pr["mergeable"] = nil
		}
		pr["mergeable_state"] = "unknown"
		pr["merged"] = false
		servePRJSON(w, pr)
	}

	// Null mergeable: miss, served as null, NOT hit-gated.
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	var pr1 map[string]any
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &pr1))
	assert.Nil(t, pr1["mergeable"], "an unresolved mergeable must be served as null")
	assert.Equal(t, false, pr1["merged"])

	// Still null upstream: the poll keeps reaching GitHub.
	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "a null cached mergeable must miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))

	// GitHub resolves: the miss absorbs the computed value...
	mergeable = "false"
	w3 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	var pr3 map[string]any
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &pr3))
	assert.Equal(t, false, pr3["mergeable"])

	// ...and the next read is a hit with the known answer, zero upstream.
	w4 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w4.Code)
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader))
	assert.Equal(t, w3.Body.String(), w4.Body.String())
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.singleHits))
	assertNoURLKeys(t, w4.Body.Bytes())
}

// TestCachedPull_WebhookNullMergeableKeepsGateHonest: a webhook upsert whose
// payload carries mergeable:null must neither clobber a known value (the
// COALESCE -- the hit keeps serving) nor un-gate an unknown one.
func TestCachedPull_WebhookNullMergeableKeepsGateHonest(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	// Absorb a resolved-mergeable PR (default fake: mergeable true).
	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	// A synchronize whose payload has mergeable:null (GitHub recomputing).
	pr := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
	pr["mergeable"] = nil
	postWebhook(t, router, "pull_request", prEvent("synchronize", pr))

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a null-mergeable webhook must not clobber the known value")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))
	var got map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &got))
	assert.Equal(t, true, got["mergeable"])

	// The inverse: a PR first seen through a webhook (no fetched mergeable)
	// must stay gated to a miss.
	pr9 := upstreamPR(9, "open", "Third PR", "hotfix", shaTree1, "2026-07-03T10:00:00Z")
	pr9["mergeable"] = nil
	postWebhook(t, router, "pull_request", prEvent("opened", pr9))
	u.single = func(w http.ResponseWriter, r *http.Request) {
		p := upstreamPR(9, "open", "Third PR", "hotfix", shaTree1, "2026-07-03T10:00:00Z")
		p["mergeable"] = true
		servePRJSON(w, p)
	}
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/9", nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "an unknown mergeable must miss even for a webhook-complete row")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
}

// TestCachedPull_BranchPushUnresolvesMergeable: a push to a PR's base branch
// makes GitHub recompute mergeability (with no webhook carrying the result),
// so the dispatcher un-resolves the cached value and the next single-PR read
// re-fetches instead of serving the pre-push answer.
func TestCachedPull_BranchPushUnresolvesMergeable(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	do(t, router, authedReq("GET", target, nil)) // absorb known mergeable
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	// Push to the PR's base branch ("main").
	postWebhook(t, router, "push", fmt.Sprintf(
		`{"ref":"refs/heads/main","before":%q,"after":%q,"repository":{"name":"repo1","owner":{"login":"org1"}}}`,
		shaBase, shaTip))

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "a base-branch push must un-resolve mergeable")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
}

// TestCachedPull_GraphQLRowIncompleteMisses: a GraphQL-sourced row -- known
// mergeable but missing the REST-only fields -- can never be rebuilt, so the
// single-PR route must miss (fetch + absorb) instead of serving a partial body.
func TestCachedPull_GraphQLRowIncompleteMisses(t *testing.T) {
	router, store, _, u := pullsCacheStack(t)

	now := time.Now()
	require.NoError(t, store.SyncOrgTruth(context.Background(), "org1", ghdata.OrgSyncData{
		Repos: []dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		PRsByRepo: map[string][]dbgen.PullRequest{"org1/repo1": {{
			Owner: "org1", Repo: "repo1", Number: 7, Title: "First PR", Url: "u",
			State: "OPEN", CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
			Mergeable:   sql.NullString{String: "MERGEABLE", Valid: true},
			AuthorLogin: sql.NullString{String: "alice", Valid: true},
		}}},
	}, testUserActor, now, now))

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a rest-incomplete row must miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))
}

// TestCachedPull_DiffAcceptPassthrough: pr-minder's getPullDiff sends the
// diff media type on this endpoint -- such requests must reach GitHub
// verbatim, every time, uncached.
func TestCachedPull_DiffAcceptPassthrough(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	rawDiff := "diff --git a/f b/f\n+x\n"
	u.single = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.diff" {
			w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
			_, _ = w.Write([]byte(rawDiff))
			return
		}
		servePRJSON(w, upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z"))
	}

	for i := 1; i <= 2; i++ {
		req := authedReq("GET", "/repos/org1/repo1/pulls/7", nil)
		req.Header.Set("Accept", "application/vnd.github.diff")
		w := do(t, router, req)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, rawDiff, w.Body.String(), "the diff representation must pass through untouched")
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.singleHits))
	}
}

// TestCachedPull_NonNumericAndQueryPassthrough: /pulls/comments (a real
// GitHub endpoint that matches the {number} pattern) and query-string
// variants are not the cached shape -- forward them.
func TestCachedPull_NonNumericAndQueryPassthrough(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/comments", nil))
	assert.Empty(t, w.Header().Get(cacheHeader), "/pulls/comments must pass through")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7?x=1", nil))
	assert.Empty(t, w2.Header().Get(cacheHeader), "query params are not modeled")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
}

// TestCachedPull_ClosedNotStored: a fetched closed PR is replayed verbatim
// (GitHub's own body, URL fields and all), never stored -- and it evicts any
// stale open row so the list stops carrying it.
func TestCachedPull_ClosedNotStored(t *testing.T) {
	router, store, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	// Absorb PR #7 while open.
	do(t, router, authedReq("GET", target, nil))
	_, _, ok, err := store.RestSinglePull(seedCtx(), "org1", "repo1", 7)
	require.NoError(t, err)
	require.True(t, ok, "open PR must be cached")

	// It closes upstream.
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamPR(7, "closed", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = nil
		pr["merged"] = true
		servePRJSON(w, pr)
	}
	// The known-mergeable row still hits until some signal moves it; a base
	// push (the usual close companion) or TTL would; simulate the direct
	// re-read after a push un-resolves it.
	postWebhook(t, router, "push", fmt.Sprintf(
		`{"ref":"refs/heads/feature","before":%q,"after":%q,"repository":{"name":"repo1","owner":{"login":"org1"}}}`,
		shaCommit, shaTip))

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader), "a closed PR is replayed verbatim, unstored")
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "closed", body["state"])
	assert.Contains(t, body, "url", "verbatim replay keeps GitHub's own fields")

	_, _, ok, err = store.RestSinglePull(seedCtx(), "org1", "repo1", 7)
	require.NoError(t, err)
	assert.False(t, ok, "absorbing a closed PR must delete the cached row")
}
