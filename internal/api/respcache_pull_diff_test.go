package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Single-PR route tests for the non-JSON reads and the closed-PR doc: the
// diff Accept and its cached 406 verdict, the shape guards, and the
// closed-then-reopened lifecycle.

func TestCachedPull_DiffAcceptPassthrough(t *testing.T) {
	router, _, db, u := pullsCacheStack(t)
	rawDiff := "diff --git a/f b/f\n+x\n"
	u.single = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.diff" {
			w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
			_, _ = w.Write([]byte(rawDiff))
			return
		}
		servePRJSON(w, upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z"))
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
		servePRJSON(w, upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z"))
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

	// The v3-suffixed spelling of the same media type shares the flow and the stored row.
	req := authedReq("GET", "/repos/org1/repo1/pulls/7", nil)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	w4 := do(t, router, req)
	require.Equal(t, http.StatusNotAcceptable, w4.Code)
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader), "both diff media-type spellings share the verdict row")

	// A multi-range Accept is not the consumer shape: plain passthrough, GitHub's body verbatim.
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

	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "before": shaBase, "after": shaTip,
		"repository": fixtureRepo(),
	})

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
		pr := upstreamSinglePR(7, "closed", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = nil
		pr["merged"] = true
		servePRJSON(w, pr)
	}
	// The known-mergeable row still hits until a signal moves it; simulate the push that un-resolves it.
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/feature", "before": shaCommit, "after": shaTip,
		"repository": fixtureRepo(),
	})

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
		pr := upstreamSinglePR(7, "closed", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = nil
		pr["merged"] = true
		servePRJSON(w, pr)
	}
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "miss", w.Header().Get(cacheHeader))

	// The cached doc keeps merged: true (GitHub's answer) and mergeable present as null.
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

	// The PR reopens: the reopened event flushes the closed doc so the next read is not stale.
	// Its payload also re-seeds the open row with an unresolved mergeable, so the read still reaches GitHub.
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = true
		pr["merged"] = false
		servePRJSON(w, pr)
	}
	reopenedPR := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
	// A reopen postdates the close it undoes; the closure record requires updated_at to prove it.
	reopenedPR["updated_at"] = "2026-07-03T10:00:00Z"
	postWebhook(t, router, "pull_request", prEvent("reopened", reopenedPR))

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "the reopened flush must force a refetch")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
	var reopened map[string]any
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &reopened))
	assert.Equal(t, "open", reopened["state"], "the fresh answer is the OPEN PR, never the stale closed doc")
	assert.Equal(t, false, reopened["merged"])

	// Steady state: the absorbed open row (known mergeable) hits; the closed doc is gone.
	w4 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.singleHits))
	_, ok, err := store.GetCachedClosedPull(seedCtx(), "org1", "repo1", 7, time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "the open absorb must drop the stale closed doc")
}

// TestCachedPull_DiffStats: the single-PR rebuild serves the additions and
// deletions the absorb has always stored (repo-nightmare renders them as each
// PR's diff size, and read them as `undefined` while the rebuild dropped
// them), and a row that has NEVER seen a single-PR answer -- absorbed from
// the LIST, which carries no stats, with mergeable resolved independently by
// a webhook -- misses rather than serving `"additions": null` forever.
func TestCachedPull_DiffStats(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls/7"

	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = true
		pr["merged"] = false
		servePRJSON(w, pr)
	}

	// Miss: fetched, absorbed, and the stats ride the rebuild.
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	var missed map[string]any
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &missed))
	assert.Equal(t, float64(12), missed["additions"])
	assert.Equal(t, float64(3), missed["deletions"])

	// Hit: served from the row, byte-identical stats.
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w2.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.singleHits))
	assert.JSONEq(t, w1.Body.String(), w2.Body.String(), "hit and miss must rebuild identically")

	// A different PR reaches this row only via the LIST (no stats) plus a webhook resolving mergeable.
	list := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=100", nil))
	require.Equal(t, http.StatusOK, list.Code)
	var listed []map[string]any
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listed))
	require.NotEmpty(t, listed)
	_, hasStats := listed[0]["additions"]
	assert.False(t, hasStats, "a rebuilt LIST must never invent stats GitHub's list does not send")

	pr8 := upstreamPR(8, "open", "Second PR", "other-branch", shaTip, "2026-07-02T10:00:00Z")
	pr8["mergeable"] = true
	postWebhook(t, router, "pull_request", prEvent("synchronize", pr8))

	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(8, "open", "Second PR", "other-branch", shaTip, "2026-07-02T10:00:00Z")
		pr["mergeable"] = true
		pr["merged"] = false
		servePRJSON(w, pr)
	}
	before := atomic.LoadInt32(&u.singleHits)
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8", nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "a statless row must not hit and serve null stats")
	assert.Equal(t, before+1, atomic.LoadInt32(&u.singleHits))

	w4 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/8", nil))
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader), "the absorbed stats must heal the row into a hit")
	assert.Equal(t, before+1, atomic.LoadInt32(&u.singleHits))
	var healed map[string]any
	require.NoError(t, json.Unmarshal(w4.Body.Bytes(), &healed))
	assert.Equal(t, float64(12), healed["additions"])
	assert.Equal(t, float64(3), healed["deletions"])
}
