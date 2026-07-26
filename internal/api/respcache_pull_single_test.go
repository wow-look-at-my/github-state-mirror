package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
		pr := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
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
	postWebhook(t, router, "push", fmt.Sprintf(
		`{"ref":"refs/heads/main","before":%q,"after":%q,"repository":{"name":"repo1","owner":{"login":"org1"}}}`,
		shaBase, shaTip))

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
		pr := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
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
	postWebhook(t, router, "push", fmt.Sprintf(
		`{"ref":"refs/heads/main","before":%q,"after":%q,"repository":{"name":"repo1","owner":{"login":"org1"}}}`,
		shaBase, shaTip))

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
func TestCachedPull_DiffAcceptPassthrough(t *testing.T) {
	router, _, db, u := pullsCacheStack(t)
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

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pull_diff406_cache`).Scan(&count))
	assert.Zero(t, count, "a 200 diff must store nothing")
}

// TestCachedPullDiff_406VerdictCached: an oversized PR's diff read earns a
// 406 "diff too large" from GitHub, and pr-minder re-earns it around every
// describe hand-off before falling back to the files API -- so the VERDICT is
// cached per PR: absorbed on the first read (rebuilt {"message": ...}, 406,
// X-GSM-Cache: miss), served from state on the second (zero upstream calls),
// flushed by the PR's own pull_request event (a head push can shrink the diff
// back under the boundary).
func TestCachedPullDiff_406VerdictCached(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		// 406 any diff-bearing Accept (single- or multi-range), like GitHub
		// answering an oversized PR's diff read.
		if strings.Contains(r.Header.Get("Accept"), ".diff") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(`{"message":"Sorry, the diff exceeded the maximum number of lines (20000)",` +
				`"documentation_url":"https://docs.github.com/rest/pulls/pulls"}`))
			return
		}
		servePRJSON(w, upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z"))
	}
	diffReq := func() *http.Request {
		req := authedReq("GET", "/repos/org1/repo1/pulls/7", nil)
		req.Header.Set("Accept", "application/vnd.github.diff")
		return req
	}

	// Miss: the 406 is absorbed and served rebuilt.
	w1 := do(t, router, diffReq())
	require.Equal(t, http.StatusNotAcceptable, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{"message":"Sorry, the diff exceeded the maximum number of lines (20000)"}`, w1.Body.String())
	assert.Equal(t, "application/json; charset=utf-8", w1.Header().Get("Content-Type"))

	// Hit: the cached verdict answers, zero upstream calls.
	w2 := do(t, router, diffReq())
	require.Equal(t, http.StatusNotAcceptable, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits), "a cached verdict must not call upstream")

	// The PR's own event flushes the verdict -- the head moved, so the diff
	// may fit again -- and the next read refetches.
	postWebhook(t, router, "pull_request",
		prEvent("synchronize", upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")))
	w3 := do(t, router, diffReq())
	require.Equal(t, http.StatusNotAcceptable, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "a pull_request event must flush the PR's 406 verdict")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))

	// The v3-suffixed spelling of the same media type shares the flow (and
	// the row the previous miss stored).
	req := authedReq("GET", "/repos/org1/repo1/pulls/7", nil)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	w4 := do(t, router, req)
	require.Equal(t, http.StatusNotAcceptable, w4.Code)
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader), "both diff media-type spellings share the verdict row")

	// A multi-range Accept is NOT the consumer shape: plain passthrough,
	// GitHub's own body verbatim (documentation_url and all).
	req = authedReq("GET", "/repos/org1/repo1/pulls/7", nil)
	req.Header.Set("Accept", "application/vnd.github.diff, application/json")
	w5 := do(t, router, req)
	require.Equal(t, http.StatusNotAcceptable, w5.Code)
	assert.Empty(t, w5.Header().Get(cacheHeader), "a multi-range Accept must pass through")
	assert.Contains(t, w5.Body.String(), "documentation_url", "the passthrough is GitHub's verbatim body")
}

// TestCachedPullDiff_PushFlushesRepoWide: a push flushes every PR's 406
// verdict for the repo -- a BASE push can move any PR's three-dot diff across
// the size boundary in either direction, with no per-PR signal.
func TestCachedPullDiff_PushFlushesRepoWide(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte(`{"message":"too large"}`))
	}
	diffReq := func() *http.Request {
		req := authedReq("GET", "/repos/org1/repo1/pulls/7", nil)
		req.Header.Set("Accept", "application/vnd.github.diff")
		return req
	}

	do(t, router, diffReq())
	w := do(t, router, diffReq())
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	postWebhook(t, router, "push", fmt.Sprintf(
		`{"ref":"refs/heads/main","before":%q,"after":%q,"repository":{"name":"repo1","owner":{"login":"org1"}}}`,
		shaBase, shaTip))

	w2 := do(t, router, diffReq())
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "a push must flush the repo's 406 verdicts")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
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

// TestCachedPull_ClosedAbsorbedAsDoc: a fetched closed PR evicts any stale
// open row (the truth table retains open PRs only) and is absorbed as a
// rendered whole-doc snapshot -- served trimmed on the miss, then replayed
// byte-identically from closed_pull_cache with zero upstream calls (every
// drain re-reads settled PRs; each read used to be a fresh passthrough).
func TestCachedPull_ClosedAbsorbedAsDoc(t *testing.T) {
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
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "the closing read absorbs the rendered doc")
	assertNoURLKeys(t, w.Body.Bytes())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "closed", body["state"])
	assert.Equal(t, true, body["merged"], "merged is GitHub's own answer, not the open rebuild's by-definition false")
	assert.Nil(t, body["mergeable"])

	_, _, ok, err = store.RestSinglePull(seedCtx(), "org1", "repo1", 7)
	require.NoError(t, err)
	assert.False(t, ok, "absorbing a closed PR must delete the cached open row")

	// The next read serves the identical doc from state, zero upstream.
	fetched := atomic.LoadInt32(&u.singleHits)
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, fetched, atomic.LoadInt32(&u.singleHits))
}

// TestCachedPull_ClosedDocReopenFlush: the closed-PR doc's lifecycle around a
// reopen. The cached doc carries GitHub's own merged answer and an EXPLICIT
// null mergeable (the key must exist); a `reopened` pull_request event
// flushes it, so the next read fetches GitHub's fresh OPEN answer instead of
// serving the stale closed snapshot -- and the open absorb keeps the doc and
// the open row mutually exclusive.
func TestCachedPull_ClosedDocReopenFlush(t *testing.T) {
	router, store, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	// A merged-closed PR: absorbed as a rendered doc on the first read.
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamPR(7, "closed", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = nil
		pr["merged"] = true
		servePRJSON(w, pr)
	}
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "miss", w.Header().Get(cacheHeader))

	// The cached doc survives with merged: true (GitHub's answer, not the
	// open rebuild's by-definition false) and mergeable PRESENT as null.
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, "hit", w2.Header().Get(cacheHeader))
	var doc map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &doc))
	assert.Equal(t, true, doc["merged"], "merged: true must survive on the merged-closed doc")
	mv, present := doc["mergeable"]
	require.True(t, present, "mergeable must be present on the closed doc (explicit null, like GitHub)")
	assert.Nil(t, mv, "a closed PR's mergeable is null")
	require.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))

	// The PR reopens: GitHub now answers open, and the reopened event must
	// flush the doc so the next read cannot serve it stale. (The event's own
	// payload also re-seeds the open row -- with an unresolved mergeable, so
	// the read still reaches GitHub.)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = true
		pr["merged"] = false
		servePRJSON(w, pr)
	}
	postWebhook(t, router, "pull_request",
		prEvent("reopened", upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")))

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "the reopened flush must force a refetch")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
	var reopened map[string]any
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &reopened))
	assert.Equal(t, "open", reopened["state"], "the fresh answer is the OPEN PR, never the stale closed doc")
	assert.Equal(t, false, reopened["merged"])

	// Steady state: the absorbed open row (known mergeable) hits, and the
	// closed doc is gone from the side table.
	w4 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
	_, ok, err := store.GetCachedClosedPull(seedCtx(), "org1", "repo1", 7, time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "the open absorb must drop the stale closed doc")
}
