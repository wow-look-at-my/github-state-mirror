package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Installation-repositories route tests. The default body is GitHub's
// documented shape, complete with the *_url templates the rebuild drops.

func defaultInstallationReposUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{
		"total_count": 1,
		"repository_selection": "selected",
		"repositories": [{
			"id": 1296269, "node_id": "MDEwOlJlcG9zaXRvcnkxMjk2MjY5",
			"name": "repo1", "full_name": "org1/repo1",
			"owner": {"login": "org1", "id": 1, "type": "Organization",
				"url": "https://api.github.com/users/org1",
				"avatar_url": "https://a", "html_url": "https://github.com/org1"},
			"private": true, "visibility": "private",
			"default_branch": "main", "fork": false,
			"archived": false, "disabled": false,
			"url": "https://api.github.com/repos/org1/repo1",
			"html_url": "https://github.com/org1/repo1",
			"hooks_url": "https://api.github.com/repos/org1/repo1/hooks",
			"git_url": "git://github.com/org1/repo1.git"
		}]
	}`))
}

const installationReposTarget = "/installation/repositories"

// installationReposReq carries a distinct bearer, exercising the per-credential key.
func installationReposReq(target, bearer string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req
}

func TestCachedInstallationRepos_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", installationReposTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.installReposHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{
		"total_count": 1,
		"repository_selection": "selected",
		"repositories": [{
			"id": 1296269, "node_id": "MDEwOlJlcG9zaXRvcnkxMjk2MjY5",
			"name": "repo1", "full_name": "org1/repo1",
			"owner": {"login": "org1", "type": "Organization"},
			"private": true, "visibility": "private",
			"default_branch": "main", "fork": false,
			"archived": false, "disabled": false
		}]
	}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", installationReposTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.installReposHits), "a hit must not call upstream")
}

// The load-bearing property: rows key the CREDENTIAL. Two bearers must never
// read each other's listing, even though the answers differ only by token --
// this is what makes a route with no reveal gate safe.
func TestCachedInstallationRepos_KeyedByCredential(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.installRepos = func(w http.ResponseWriter, r *http.Request) {
		writeGitHubJSON(w, map[string]any{
			"total_count": 1, "repository_selection": "selected",
			"repositories": []any{map[string]any{
				"id": 1, "node_id": "R_1", "name": "r",
				"full_name": "org1/" + r.Header.Get("Authorization"),
				"owner":     map[string]any{"login": "org1", "type": "Organization"},
				"private":   true, "visibility": "private", "default_branch": "main", "fork": false,
				"archived": false, "disabled": false, "url": "https://api.github.com/x",
			}},
		})
	}

	first := do(t, router, installationReposReq(installationReposTarget, "tok-a"))
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, "miss", first.Header().Get(cacheHeader))

	second := do(t, router, installationReposReq(installationReposTarget, "tok-b"))
	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, "miss", second.Header().Get(cacheHeader),
		"a second credential must never be answered from the first's row")
	assert.NotEqual(t, first.Body.String(), second.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.installReposHits))

	// Each credential then serves from its own row.
	assert.Equal(t, "hit", do(t, router, installationReposReq(installationReposTarget, "tok-a")).Header().Get(cacheHeader))
	assert.Equal(t, "hit", do(t, router, installationReposReq(installationReposTarget, "tok-b")).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.installReposHits))
}

// Pages are separate rows, and an unmodeled shape forwards with a counted
// reason rather than minting one.
func TestCachedInstallationRepos_ShapeGuards(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", installationReposTarget+"?per_page=100&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, authedReq("GET", installationReposTarget+"?per_page=100&page=2", nil)).Header().Get(cacheHeader),
		"page 2 must not be answered by page 1's row")
	assert.Equal(t, "hit", do(t, router, authedReq("GET", installationReposTarget+"?per_page=100&page=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.installReposHits))

	for _, target := range []string{
		installationReposTarget + "?per_page=0",                                              // out of range
		installationReposTarget + "?per_page=101",                                            // out of range
		fmt.Sprintf("%s?page=%d", installationReposTarget, installationReposMaxCachedPage+1), // past the modeled depth
		installationReposTarget + "?sort=created",                                            // unmodeled parameter
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "%s must forward", target)
	}
	assert.Equal(t, int32(6), atomic.LoadInt32(&u.installReposHits))
}

// An installation event says some installation's repository set moved. Rows
// key a credential, so the only honest flush is the whole table.
func TestCachedInstallationRepos_InstallationEventFlushes(t *testing.T) {
	for _, event := range []string{"installation", "installation_repositories"} {
		t.Run(event, func(t *testing.T) {
			router, _, _, u := respCacheStack(t)

			require.Equal(t, "miss", do(t, router, authedReq("GET", installationReposTarget, nil)).Header().Get(cacheHeader))
			require.Equal(t, "hit", do(t, router, authedReq("GET", installationReposTarget, nil)).Header().Get(cacheHeader))

			postWebhook(t, router, event, `{"action":"added","installation":{"id":42}}`)

			assert.Equal(t, "miss", do(t, router, authedReq("GET", installationReposTarget, nil)).Header().Get(cacheHeader),
				event+" must flush the cached listings")
			assert.Equal(t, int32(2), atomic.LoadInt32(&u.installReposHits))
		})
	}
}

// A non-200 is relayed unstored: an expired installation token must not leave
// a 401 answering for the next 15 minutes.
func TestCachedInstallationRepos_NonOKRelayedUnstored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.installRepos = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", installationReposTarget, nil))
		require.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.installReposHits))
	}
}
