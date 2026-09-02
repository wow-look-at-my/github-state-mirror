package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeable_state was parsed off upstream and then dropped by the rebuild, on
// every path -- the rebuilt document is what the MISS path serves too, so no
// fetch ever healed it and a consumer simply could not read the field. It is
// the only place GitHub says a strict up-to-date rule is what blocks a merge
// ("behind"), so its absence is what pushed pr-minder onto inferring
// behindness from a compare, an answer this cache can serve stale.

// TestRenderedSinglePullCarriesConsumerFields pins the superset promise the
// rebuilt shape makes. The fields listed here are read off mirror-served PR
// objects by pr-minder; a rebuild that omits hands back undefined, which
// is indistinguishable from "GitHub said nothing" and fails silently forever.
func TestRenderedSinglePullCarriesConsumerFields(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"], pr["mergeable_state"], pr["merged"] = true, "behind", false
		servePRJSON(w, pr)
	}

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls/7", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	for _, key := range []string{
		"number", "state", "draft", "title", "body", "node_id", "user", "labels",
		"head", "base", "auto_merge", "merge_commit_sha", "created_at", "updated_at",
		"mergeable", "mergeable_state", "merged", "additions", "deletions",
	} {
		_, ok := got[key]
		assert.Truef(t, ok, "the rebuilt single-PR document dropped %q; a consumer reading it gets undefined, on every path", key)
	}
	assert.Equal(t, "behind", got["mergeable_state"], "the state must survive the rebuild verbatim -- 'behind' is the whole point")
}

// TestSinglePullMergeableStateSurvivesHitAndMiss: the miss serves the rebuild
// too, so hit and miss must agree byte for byte. A field present on only
// of them is the worse bug -- it works until the cache warms.
func TestSinglePullMergeableStateSurvivesHitAndMiss(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"], pr["mergeable_state"], pr["merged"] = true, "behind", false
		servePRJSON(w, pr)
	}
	target := "/repos/org1/repo1/pulls/7"

	miss := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, miss.Code)
	require.Equal(t, "miss", miss.Header().Get(cacheHeader))

	hit := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, hit.Code)
	assert.Equal(t, "hit", hit.Header().Get(cacheHeader), "a resolved mergeable + mergeable_state is a cacheable answer")
	assert.Equal(t, miss.Body.String(), hit.Body.String(), "hit and miss must be the same document")
}

// TestSinglePullUnresolvedStateKeepsMissing: "unknown" is GitHub's word for
// "still computing", not an answer. Caching it would serve a resolved-looking
// document that nothing re-asks -- and mergeable_state is the field with the
// weakest invalidation, so it must gate the hit rather than ride along.
func TestSinglePullUnresolvedStateKeepsMissing(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	state := "unknown"
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamSinglePR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"], pr["mergeable_state"], pr["merged"] = true, state, false
		servePRJSON(w, pr)
	}
	target := "/repos/org1/repo1/pulls/7"

	for i := 0; i < 2; i++ {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an unresolved mergeable_state must keep reaching GitHub")
		var got map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.Equal(t, "unknown", got["mergeable_state"], "and it is served as GitHub serves it: unknown, meaning ask again")
	}

	// GitHub resolves it: the answer becomes cacheable.
	state = "clean"
	require.Equal(t, "miss", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	w := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "clean", got["mergeable_state"])
}
