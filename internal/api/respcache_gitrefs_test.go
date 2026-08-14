package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Git-ref route tests (GET /repos/{owner}/{repo}/git/ref/{ref...}); the
// shared fake upstream (respCacheUpstream) lives in respcache_test.go.

// defaultGitRefUpstream answers with a GitHub-shaped ref object full of URL
// fields, so the tests can prove the rebuild drops them.
func defaultGitRefUpstream(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimPrefix(r.URL.Path, "/repos/org1/repo1/git/ref/")
	writeGitHubJSON(w, map[string]any{
		"ref":     "refs/" + strings.TrimPrefix(ref, "refs/"),
		"node_id": "REF_kwAE",
		"url":     "https://api.github.com/repos/org1/repo1/git/refs/" + ref,
		"object": map[string]any{
			"sha": shaTip, "type": "commit",
			"url": "https://api.github.com/repos/org1/repo1/git/commits/" + shaTip,
		},
	})
}

// The core flow: fetch + absorb (miss), then serve the byte-identical stored
// doc (hit, zero upstream calls), with every URL field dropped.
func TestCachedGitRef_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitRefHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString(map[string]any{
		"ref": "refs/heads/main", "node_id": "REF_kwAE",
		"object": map[string]any{"sha": shaTip, "type": "commit"},
	}), w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitRefHits), "a hit must not call upstream")
}

// A branch name with slashes must survive the greedy wildcard intact — the
// whole reason this route is not a single-segment {ref} parameter.
func TestCachedGitRef_SlashedBranchName(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/claude/some/deep-branch"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Contains(t, w1.Body.String(), "refs/heads/claude/some/deep-branch")

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitRefHits))
}

// A push APPLIES its own tip to every spelling of the ref, and costs ZERO
// upstream calls doing it. The payload states `after` outright, so a flush
// here would delete a row only to buy the identical sha back over HTTP on the
// next read -- the apply-don't-invalidate rule (CLAUDE.md). Rows key the
// verbatim requested ref, so all three spellings must move together.
func TestCachedGitRef_PushAppliesTipToEverySpelling(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	spellings := []string{
		"/repos/org1/repo1/git/ref/heads/main",
		"/repos/org1/repo1/git/ref/refs/heads/main",
	}
	fetched := map[string]string{}
	for _, target := range spellings {
		do(t, router, authedReq("GET", target, nil))
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, "hit", w.Header().Get(cacheHeader))
		fetched[target] = w.Body.String()
	}
	before := atomic.LoadInt32(&u.gitRefHits)

	// The fake upstream answers shaTip, so pushing shaCommit proves the served
	// sha came from the PAYLOAD and not from a refetch.
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/main", "after": shaCommit, "repository": fixtureRepo(),
	})

	for _, target := range spellings {
		w := do(t, router, authedReq("GET", target, nil))
		assert.Equal(t, "hit", w.Header().Get(cacheHeader), "the applied tip must serve from cache: %s", target)
		assert.Contains(t, w.Body.String(), shaCommit, "%s must serve the pushed tip", target)
		assert.NotContains(t, w.Body.String(), shaTip, "%s must not still serve the pre-push tip", target)
		// Byte-identical to the fetched answer but for the sha: the applied
		// doc is re-marshalled in the storage layer, so a field-order drift
		// between there and the route's render would surface right here.
		assert.Equal(t, strings.Replace(fetched[target], shaTip, shaCommit, 1), w.Body.String(),
			"an applied tip must be byte-indistinguishable from a fetched one: %s", target)
	}
	assert.Equal(t, before, atomic.LoadInt32(&u.gitRefHits),
		"applying the payload's own tip must cost no upstream calls")
}

// A DELETION states no tip (all-zeros `after`), so there is nothing to apply
// and the row must go -- invalidation is still correct where the payload
// cannot answer.
func TestCachedGitRef_PushDeletionFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"
	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	before := atomic.LoadInt32(&u.gitRefHits)

	postWebhook(t, router, "push", `{"ref":"refs/heads/main","deleted":true,
		"after":"0000000000000000000000000000000000000000",
		"repository":{"name":"repo1","owner":{"login":"org1"},"default_branch":"main"}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader),
		"a deletion carries no tip to apply, so the row must be dropped")
	assert.Equal(t, before+1, atomic.LoadInt32(&u.gitRefHits))
}

// A push to a DIFFERENT branch must not flush this one: the whole point of
// keying per ref rather than per repo.
func TestCachedGitRef_OtherBranchPushKeepsHit(t *testing.T) {
	router, _, _, _ := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"

	do(t, router, authedReq("GET", target, nil))
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/other", "after": shaMid, "repository": fixtureRepo(),
	})

	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "an unrelated branch's push must not flush this ref")
}

// The 404 absent-ref VERDICT is absorbed (deleted heads are re-polled
// forever) and cleared by the push that recreates the ref.
func TestCachedGitRef_AbsentVerdictCachedThenClearedByCreate(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.gitRef = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`))
	}
	target := "/repos/org1/repo1/git/ref/heads/gone"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes(), "documentation_url")
	assert.JSONEq(t, `{"message":"Not Found","status":"404"}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "the absent-ref verdict must be replayed")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.gitRefHits))

	// The branch is created: the push carrying that ref must drop the verdict.
	u.gitRef = defaultGitRefUpstream
	postWebhookJSON(t, router, "push", map[string]any{
		"ref": "refs/heads/gone", "after": shaTip, "repository": fixtureRepo(),
	})

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code, "the verdict must not outlive the ref's creation")
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
}

// Shape guards: a partial ref (which GitHub answers with an ARRAY, a
// different shape), any query parameter, and a non-default Accept all pass
// through rather than being modeled wrongly.
func TestCachedGitRef_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, _ := respCacheStack(t)

	for _, tc := range []struct {
		name   string
		target string
		accept string
	}{
		{"partial ref answers an array", "/repos/org1/repo1/git/ref/heads", ""},
		{"unqualified ref", "/repos/org1/repo1/git/ref/main", ""},
		{"unknown ref namespace", "/repos/org1/repo1/git/ref/pull/7/head", ""},
		{"query parameter", "/repos/org1/repo1/git/ref/heads/main?per_page=1", ""},
		{"non-default accept", "/repos/org1/repo1/git/ref/heads/main", "application/vnd.github.raw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedReq("GET", tc.target, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			w := do(t, router, req)
			assert.Empty(t, w.Header().Get(cacheHeader), "must be forwarded, not served from the cache")
		})
	}
}

func TestGitRefCacheable(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"heads/main", true},
		{"refs/heads/main", true},
		{"heads/claude/a/b", true},
		{"tags/v1.2.3", true},
		{"refs/tags/v1", true},
		{"heads", false},
		{"heads/", false},
		{"main", false},
		{"", false},
		{"pull/7/head", false},
		{"refs/pull/7/head", false},
	} {
		assert.Equal(t, tc.want, gitRefCacheable(tc.ref), "ref %q", tc.ref)
	}
}

// ---- stale-tip repair (docs/cache/stale-tip-repair.md) ----

// openPRWithBase delivers an OPEN PR whose base names `branch` at `baseSHA` --
// the second, independently-absorbed answer about where that branch is.
func openPRWithBase(t *testing.T, router http.Handler, number int, branch, baseSHA, headSHA string) {
	t.Helper()
	postWebhookJSON(t, router, "pull_request", map[string]any{
		"action": "synchronize", "number": number, "repository": fixtureRepo(),
		"pull_request": map[string]any{
			"number": number, "node_id": "PR_kwAE", "state": "open", "title": "t", "html_url": "u",
			"base": map[string]any{"ref": branch, "sha": baseSHA, "repo": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}}},
			"head": map[string]any{"ref": "feature", "sha": headSHA},
		},
	})
}

// seedRefRow stores a ref answer as if it had been fetched `ago` in the past.
// Stored timestamps are whole seconds, so a row fetched in the same second as
// the PR view that contradicts it is deliberately NOT contradicted -- the
// evidence has to be newer than our own answer. Back-dating the row is how
// these tests get that ordering without sleeping through a second boundary.
func seedRefRow(t *testing.T, store *ghdata.Store, ref, sha string, ago time.Duration) {
	t.Helper()
	doc, err := marshalTrimmed(gitRefJSON{
		Ref: "refs/" + strings.TrimPrefix(ref, "refs/"), NodeID: "REF_kwAE",
		Object: gitRefObjJSON{SHA: sha, Type: "commit"},
	})
	require.NoError(t, err)
	require.NoError(t, store.PutCachedGitRef(context.Background(), ghdata.CachedGitRef{
		Owner: "org1", Repo: "repo1", Ref: ref, Status: http.StatusOK, Doc: string(doc),
	}, time.Now().Add(-ago), gitRefCacheTTL))
}

// The mirror must not serve a tip its own PR rows contradict. A lost push
// leaves the ref row wrong; the PR's base.sha arrives by another delivery and
// another route and says otherwise; the mirror does not pick a side, it asks
// GitHub -- with no client telling it to.
func TestCachedGitRef_ContradictingPRBaseIsRefetchedNotServed(t *testing.T) {
	router, store, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"

	// A row fetched a minute ago, from before the branch moved.
	seedRefRow(t, store, "heads/main", shaTip, time.Minute)
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	before := atomic.LoadInt32(&u.gitRefHits)

	// The branch moved and the push delivery never arrived -- but a PR based on
	// it did, naming the new tip.
	u.gitRef = func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, map[string]any{
			"ref": "refs/heads/main", "node_id": "REF_kwAE",
			"object": map[string]any{"sha": shaCommit, "type": "commit"},
		})
	}
	openPRWithBase(t, router, 10, "main", shaCommit, shaMid)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a contradicted row must not be served")
	assert.Contains(t, w.Body.String(), shaCommit)
	assert.Equal(t, before+1, atomic.LoadInt32(&u.gitRefHits))

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "the refetched row settles it")
	assert.Contains(t, w2.Body.String(), shaCommit)
	assert.Equal(t, before+1, atomic.LoadInt32(&u.gitRefHits))
}

// A PR's base.sha LAGS its branch by design, so most disagreements are lag,
// not staleness. The mirror still asks once -- it cannot tell the two apart
// without asking -- but exactly once: re-absorbing the same lagging sha is not
// new evidence, and the row it already paid for stands.
func TestCachedGitRef_LaggingPRBaseCostsOneRefetchNotEvery(t *testing.T) {
	router, store, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"
	seedRefRow(t, store, "heads/main", shaTip, time.Minute)
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	before := atomic.LoadInt32(&u.gitRefHits)

	// Upstream keeps answering shaTip: the ref row was right and the PR's
	// base.sha is simply behind.
	openPRWithBase(t, router, 11, "main", shaMid, shaCommit)
	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader), "the first disagreement is worth one question")
	assert.Equal(t, before+1, atomic.LoadInt32(&u.gitRefHits))

	for i := 0; i < 3; i++ {
		openPRWithBase(t, router, 11, "main", shaMid, shaCommit)
		assert.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader),
			"re-absorbing the same lagging base.sha must not re-trigger (round %d)", i)
	}
	assert.Equal(t, before+1, atomic.LoadInt32(&u.gitRefHits), "exactly one refetch for one disagreeing sha")
}

// Evidence older than the row's own fetch was already accounted for by
// fetching: a PR absorbed BEFORE the ref read cannot contradict it.
func TestCachedGitRef_PRBaseOlderThanTheRowIsNotEvidence(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"

	openPRWithBase(t, router, 12, "main", shaCommit, shaMid)
	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	before := atomic.LoadInt32(&u.gitRefHits)

	assert.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	assert.Equal(t, before, atomic.LoadInt32(&u.gitRefHits), "the row was fetched after that PR view; nothing to re-ask")
}

// A PR on a DIFFERENT branch says nothing about this ref.
func TestCachedGitRef_OtherBranchPRIsNotEvidence(t *testing.T) {
	router, store, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"
	seedRefRow(t, store, "heads/main", shaTip, time.Minute)
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	before := atomic.LoadInt32(&u.gitRefHits)

	openPRWithBase(t, router, 13, "release", shaCommit, shaMid)
	assert.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	assert.Equal(t, before, atomic.LoadInt32(&u.gitRefHits))
}

// A merge is delivered twice -- as a push, and as pull_request/closed. The
// second one states the base branch's new tip too, so losing the push alone no
// longer leaves the tip wrong.
func TestCachedGitRef_MergedPullRequestAppliesBaseTip(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"
	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	before := atomic.LoadInt32(&u.gitRefHits)

	postWebhookJSON(t, router, "pull_request", map[string]any{
		"action": "closed", "number": 11, "repository": fixtureRepo(),
		"pull_request": map[string]any{
			"number": 11, "state": "closed", "merged": true,
			"merged_at":        time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"merge_commit_sha": shaCommit,
			"base":             map[string]any{"ref": "main", "sha": shaTip, "repo": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}}},
			"head":             map[string]any{"ref": "feature", "sha": shaMid},
		},
	})

	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Contains(t, w.Body.String(), shaCommit, "the merge commit is the base branch's new tip")
	assert.Equal(t, before, atomic.LoadInt32(&u.gitRefHits), "applying the payload must cost no upstream call")
}

// The two ways a pull_request delivery states NOTHING about the base tip. An
// open PR's merge_commit_sha is the throwaway test-merge, which is on no
// branch; and a merge that predates the row's own content is a view of a
// moment the row already accounts for.
func TestCachedGitRef_UnmergedAndLatePullRequestsLeaveTheTip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		merged   bool
		mergedAt time.Time
	}{
		{"open PR's test-merge commit", false, time.Now().Add(time.Minute)},
		{"merge older than the stored row", true, time.Now().Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, _, u := respCacheStack(t)
			target := "/repos/org1/repo1/git/ref/heads/main"
			do(t, router, authedReq("GET", target, nil))
			require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
			before := atomic.LoadInt32(&u.gitRefHits)

			state := "open"
			if tc.merged {
				state = "closed"
			}
			postWebhookJSON(t, router, "pull_request", map[string]any{
				"action": "synchronize", "number": 12, "repository": fixtureRepo(),
				"pull_request": map[string]any{
					"number": 12, "state": state, "merged": tc.merged,
					"merged_at":        tc.mergedAt.UTC().Format(time.RFC3339),
					"merge_commit_sha": shaCommit,
					"base":             map[string]any{"ref": "main", "sha": shaTip, "repo": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}}},
					"head":             map[string]any{"ref": "feature", "sha": shaMid},
				},
			})

			w := do(t, router, authedReq("GET", target, nil))
			assert.Equal(t, "hit", w.Header().Get(cacheHeader))
			assert.Contains(t, w.Body.String(), shaTip, "the stored tip must survive")
			assert.NotContains(t, w.Body.String(), shaCommit)
			assert.Equal(t, before, atomic.LoadInt32(&u.gitRefHits))
		})
	}
}
