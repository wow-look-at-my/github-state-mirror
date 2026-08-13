package api

import (
	"context"
	"database/sql"
	"encoding/json"
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
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		// mergeable_state tracks mergeable, as GitHub's does: "unknown" while
		// the test merge computes, a real state once it resolves. The hit gate
		// reads both, so pinning it to "unknown" would hold every read on the
		// miss path and prove nothing about caching.
		switch mergeable {
		case "true":
			pr["mergeable"], pr["mergeable_state"] = true, "clean"
		case "false":
			pr["mergeable"], pr["mergeable_state"] = false, "dirty"
		default:
			pr["mergeable"], pr["mergeable_state"] = nil, "unknown"
		}
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
		p := upstreamSinglePR(9, "open", "Third PR", "hotfix", shaTree1, "2026-07-03T10:00:00Z")
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
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(),
	})

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "a base-branch push must un-resolve mergeable")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
}

// TestCachedPull_StaleShaRefetchNeverReresolves: the webhooks#66 frozen-sha
// scenario end to end. A base push un-resolves the row AND remembers the
// invalidated test-merge sha; GitHub's recompute lags, so the refetch
// re-offers the SAME sha with a resolved mergeable -- a pre-push answer by
// definition (a tip change always changes the test-merge sha). The mirror
// must NOT re-resolve from it (the old behavior: one lagged refetch
// re-resolved the stale sha and every later read was a hit serving it frozen,
// never touching GitHub again): the answer is stored AND served unresolved,
// every poll keeps missing -- each miss re-triggering GitHub's recompute --
// until GitHub serves a NEW sha, which resolves the row and hits again.
func TestCachedPull_StaleShaRefetchNeverReresolves(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	offeredSha := shaMid // the pre-push test-merge sha (upstreamPR's default)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = true
		pr["merged"] = false
		pr["merge_commit_sha"] = offeredSha
		servePRJSON(w, pr)
	}

	// Absorb the resolved pre-push answer; it serves as a hit.
	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	// The PR's base branch moves.
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(),
	})

	// GitHub's recompute lags: it re-offers the SAME sha, still "resolved".
	// The mirror rejects it -- miss, served unresolved, stored unresolved.
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader))
	var body2 map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &body2))
	assert.Nil(t, body2["mergeable"], "a pre-push answer must be served unresolved")
	assert.Nil(t, body2["merge_commit_sha"], "the provably-stale sha must not be served")

	// The poll KEEPS reaching GitHub: the rejected answer re-resolved nothing.
	w3 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "a re-offered invalidated sha must never re-resolve the row")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.singleHits))

	// GitHub's recompute lands: a NEW sha resolves the row on the next miss...
	offeredSha = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	w4 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader))
	var body4 map[string]any
	require.NoError(t, json.Unmarshal(w4.Body.Bytes(), &body4))
	assert.Equal(t, true, body4["mergeable"])
	assert.Equal(t, offeredSha, body4["merge_commit_sha"], "the fresh sha serves")

	// ...and the next read is a hit again, serving the fresh answer.
	w5 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w5.Header().Get(cacheHeader))
	assert.Equal(t, w4.Body.String(), w5.Body.String())
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.singleHits))
	assertNoURLKeys(t, w5.Body.Bytes())
}

// TestCachedPull_PostPushProvenAnswerHealsWrongMark is the mirror image of
// TestCachedPull_StaleShaRefetchNeverReresolves: the wrong-mark race, end to
// end. GitHub recomputes mergeability within seconds of a push once a read
// triggers it, and pr-minder polls right after pushing -- so a poll-driven
// miss absorbs GitHub's POST-push answer (base tip already at the push's
// after) BEFORE the push delivery reaches the mirror, and the late delivery
// then stamps that FRESH sha stale. Pre-fix the route served mergeable:null
// for up to MergeStaleTTL (an hour) while github.com showed the PR computed.
// Now the marker carries the push's after tip: the next poll's answer
// demonstrates it post-dates the push (its base tip matches), so it is
// accepted, served RESOLVED, and the row hits again -- healed on the very
// next poll.
func TestCachedPull_PostPushProvenAnswerHealsWrongMark(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	baseTip := shaTip // GitHub's reported base tip: already the push's after
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = true
		pr["merged"] = false
		pr["merge_commit_sha"] = shaMid
		pr["base"].(map[string]any)["sha"] = baseTip
		servePRJSON(w, pr)
	}

	// The poll-driven miss absorbs GitHub's post-push answer first...
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))

	// ...then the LATE push delivery lands and wrongly marks the fresh sha
	// (the push whose after IS the base tip the answer already reported).
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(),
	})

	// The next poll re-offers the SAME sha -- with the base tip equal to the
	// push's after: post-push proof, accepted, served RESOLVED.
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader))
	var body2 map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &body2))
	assert.Equal(t, true, body2["mergeable"], "a post-push-proven answer must be served resolved")
	assert.Equal(t, shaMid, body2["merge_commit_sha"], "the wrongly-marked sha serves once proven")

	// And the row re-resolved: the next read is a hit, zero further upstream.
	w3 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader), "the healed row must hit again")
	assert.Equal(t, w2.Body.String(), w3.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
	assertNoURLKeys(t, w3.Body.Bytes())
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
// diff media type on this endpoint. A 200 diff BODY is deliberately never
// stored (verbatim byte caching is rejected doctrine) -- so a diff that fits
// under GitHub's size boundary reaches GitHub and relays untouched, EVERY
// time, byte-identically, with the diff Accept forwarded upstream. Only the
// 406 verdict is cached (TestCachedPullDiff_406VerdictCached).
