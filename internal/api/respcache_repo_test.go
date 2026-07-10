package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Bare-repo route tests (GET /repos/{owner}/{repo}). This route has no
// snapshot table: it rebuilds from the repos TRUTH row, so the fake
// upstream's bare-repo endpoint (u.probe) answers BOTH the reveal probe and
// this route's miss fetches -- probeHits counts them together.

// TestCachedRepo_SeededPublicRowHit: a complete public truth row (e.g.
// webhook-maintained) serves the trimmed rebuild with ZERO upstream calls --
// no reveal probe (public fast path) and no fetch.
func TestCachedRepo_SeededPublicRowHit(t *testing.T) {
	router, store, _, u := respCacheStack(t)

	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "org1", Name: "pub", NameWithOwner: "org1/pub",
		Url: "https://github.com/org1/pub", Visibility: ghdata.VisibilityPublic,
		DefaultBranch: sql.NullString{String: "main", Valid: true},
		OwnerLogin:    sql.NullString{String: "org1", Valid: true},
	}))

	w := do(t, router, authedReq("GET", "/repos/org1/pub", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "a complete truth row serves without upstream")
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.probeHits), "public fast path: no probe, no fetch")
	assertNoURLKeys(t, w.Body.Bytes())
	assert.JSONEq(t, `{
		"name": "pub", "full_name": "org1/pub", "owner": {"login": "org1"},
		"private": false, "visibility": "public", "default_branch": "main",
		"archived": false, "disabled": false
	}`, w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body, 8, "the rebuild emits exactly the eight modeled keys (no url, no pushed_at, no fork/id)")
}

// TestCachedRepo_PrivateProbeAbsorbsThenHit: a private repo's first touch
// pays exactly ONE upstream call -- the reveal probe, whose 200 body absorbs
// the repository object into truth -- and the handler then serves from that
// just-absorbed row as a HIT. The grant is remembered, so later reads stay
// probe-free.
func TestCachedRepo_PrivateProbeAbsorbsThenHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w := do(t, router, authedReq("GET", "/repos/org1/repo1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "the probe-absorbed row answers the read itself")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits), "exactly one upstream call: the reveal probe")
	assertNoURLKeys(t, w.Body.Bytes())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "org1/repo1", body["full_name"])
	assert.Equal(t, true, body["private"], "internal-or-private folds to private=true")
	assert.Equal(t, "private", body["visibility"])
	assert.Equal(t, "main", body["default_branch"])

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits), "grants are remembered; no re-probe, no fetch")
}

// TestCachedRepo_IncompleteRowFetchesAndHeals: a row that cannot answer the
// rebuild (here: public but no known default branch, the GraphQL-seeded
// shape) falls to the fetch path, whose absorbed 200 heals the row -- the
// next read hits with no further upstream calls.
func TestCachedRepo_IncompleteRowFetchesAndHeals(t *testing.T) {
	router, store, _, u := respCacheStack(t)

	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u",
		Visibility: ghdata.VisibilityPublic,
	}))

	w := do(t, router, authedReq("GET", "/repos/org1/repo1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an incomplete truth row must fetch")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits),
		"one upstream call: the miss fetch (no reveal probe -- the row is public)")

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1", nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "the absorbed answer heals the row")
	assert.Equal(t, w.Body.String(), w2.Body.String(), "hit and miss serve the identical trimmed shape")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits), "no further upstream calls")
}

// TestCachedRepo_QueryPassthrough: query params are not modeled -- the
// request is forwarded to GitHub verbatim, uncached.
func TestCachedRepo_QueryPassthrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w := do(t, router, authedReq("GET", "/repos/org1/repo1?x=1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader), "query params must pass through")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits), "forwarded to GitHub verbatim")
	assert.Contains(t, w.Body.String(), "html_url", "the passthrough is GitHub's verbatim body")
}

// TestCachedRepo_Upstream404Passthrough: a non-200 fetch answer (truth still
// says public but GitHub deleted the repo) is relayed verbatim -- GitHub's
// own body, no cache marker -- and stores nothing, so every read re-asks.
func TestCachedRepo_Upstream404Passthrough(t *testing.T) {
	router, store, _, u := respCacheStack(t)

	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "org1", Name: "gone", NameWithOwner: "org1/gone", Url: "u",
		Visibility: ghdata.VisibilityPublic,
	}))
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", "/repos/org1/gone", nil))
		require.Equal(t, http.StatusNotFound, w.Code, "the 404 must be relayed")
		assert.Empty(t, w.Header().Get(cacheHeader), "a non-200 must be replayed unstored")
		assert.Contains(t, w.Body.String(), "documentation_url", "GitHub's verbatim answer, not a trimmed rebuild")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.probeHits), "nothing stored; every read fetches")
	}
}
