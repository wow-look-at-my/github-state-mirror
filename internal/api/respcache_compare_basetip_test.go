package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The base-tip belt. A comparison's `behind_by` is the answer whose staleness
// stops its own repair: pr-minder reads it to decide whether a PR's branch
// needs updating, so a stale "not behind" ends the work that would have
// produced a fresh answer. Every test here therefore moves the base branch
// WITHOUT running the flush -- exactly the state a missed push delivery
// leaves -- and asserts on what the route serves next.

// setKnownTip writes a ref answer for `main` directly, standing in for the
// tip a push would have applied. It deliberately does not touch the compare
// rows: the flush is what these tests are proving the route no longer needs.
func setKnownTip(t *testing.T, store *ghdata.Store, sha string) {
	t.Helper()
	require.NoError(t, store.PutCachedGitRef(context.Background(), ghdata.CachedGitRef{
		Owner: "org1", Repo: "repo1", Ref: "heads/main", Status: http.StatusOK,
		Doc: mustJSONString(map[string]any{
			"ref": "refs/heads/main", "node_id": "REF_kwAE",
			"object": map[string]any{"sha": sha, "type": "commit"},
		}),
	}, time.Now(), ghdata.GitRefCacheTTL))
}

func TestCachedCompare_MovedBaseBranchIsNotServed(t *testing.T) {
	router, store, _, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))

	// Move the branch off shaBase, the tip the absorbed answer was computed against.
	setKnownTip(t, store, shaCommit)

	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader),
		"a comparison against a base tip the branch has left must not be served")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.compareHits), "the miss must re-ask GitHub")
}

func TestCachedCompare_UnmovedBaseBranchStillServes(t *testing.T) {
	router, store, _, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	setKnownTip(t, store, shaBase) // the tip it was computed against

	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader),
		"knowing the tip and finding it unchanged is the strongest reason to serve")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))
}

// No ref answer for the base branch means no information, which is not the
// same as evidence of a move. Serving there is the pre-existing contract
// (flush, then TTL); refusing would make every comparison a permanent
// miss without making any of them fresher.
func TestCachedCompare_UnknownBaseTipStillServes(t *testing.T) {
	router, _, _, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))
}

// A sha on the base side names a commit, and a commit does not move. The
// check must not read `main`'s tip and reject an answer that never depended
// on it.
func TestCachedCompare_ShaBaseIgnoresBranchMovement(t *testing.T) {
	router, store, _, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/" + shaBase + "...dev"

	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	setKnownTip(t, store, shaCommit)

	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "an immutable base cannot go stale")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.compareHits))
}

// The tip is read across every spelling the ref route keys, because the row
// that carries it is whichever the caller happened to ask for.
func TestCachedCompare_MovedBaseDetectedFromAnyRefSpelling(t *testing.T) {
	for _, spelling := range []string{"main", "heads/main", "refs/heads/main"} {
		t.Run(spelling, func(t *testing.T) {
			router, store, _, u := compareCacheStack(t)
			target := "/repos/org1/repo1/compare/main...dev"

			require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
			require.NoError(t, store.PutCachedGitRef(context.Background(), ghdata.CachedGitRef{
				Owner: "org1", Repo: "repo1", Ref: spelling, Status: http.StatusOK,
				Doc: mustJSONString(map[string]any{
					"ref": "refs/heads/main", "node_id": "REF_kwAE",
					"object": map[string]any{"sha": shaCommit, "type": "commit"},
				}),
			}, time.Now(), ghdata.GitRefCacheTTL))

			assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
			assert.Equal(t, int32(2), atomic.LoadInt32(&u.compareHits))
		})
	}
}

// The push's own tip is the input this belt exists to consume: applying it to
// the ref rows is enough to retire every comparison against that branch, with
// no compare-side flush involved at all.
func TestCachedCompare_AppliedPushTipRetiresTheComparison(t *testing.T) {
	router, store, _, u := compareCacheStack(t)
	target := "/repos/org1/repo1/compare/main...dev"

	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	setKnownTip(t, store, shaBase)
	require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))

	applied, err := store.ApplyPushedRefTip(context.Background(), "org1", "repo1", "heads/main",
		shaCommit, time.Now(), ghdata.GitRefCacheTTL)
	require.NoError(t, err)
	require.True(t, applied)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.compareHits))
}
