package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Verdicts and query shapes on routes that were already cached: the answers
// the brief listed as unmodeled-response or unmodeled-query.

func writeStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// It is stored for that caller, so the next poll does not reach GitHub.
func TestCachedCommitCI_ForbiddenIsPerCaller(t *testing.T) {
	router, _, _, u := commitCIStack(t)
	u.status = func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusForbidden, `{"message":"Resource not accessible by personal access token","status":"403"}`)
	}
	target := "/repos/org1/repo1/commits/" + shaTip + "/status?per_page=100"
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.statusHits))

	// Deeper pages are modeled up to the cap.
	assert.Equal(t, "miss", do(t, router, authedReq("GET", "/repos/org1/repo1/commits/main/check-runs?per_page=100&page=12", nil)).Header().Get(cacheHeader))
}

func TestCachedReadme_LegalBlockIsStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.readme = func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusUnavailableForLegalReasons,
			`{"message":"Repository access blocked","block":{"reason":"dmca","created_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/github/dmca"}}`)
	}
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/readme", nil))
		require.Equal(t, http.StatusUnavailableForLegalReasons, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
		assertNoURLKeys(t, w.Body.Bytes())
		assert.JSONEq(t, `{"message":"Repository access blocked","block":{"reason":"dmca","created_at":"2026-01-01T00:00:00Z"}}`, w.Body.String())
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.readmeHits))
}

// A submodule entry is a contents answer of its own type, stored and rebuilt.
func TestCachedContents_SubmoduleIsStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.contents = func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, map[string]any{
			"type": "submodule", "submodule_git_url": "https://github.com/org1/dep.git",
			"size": 0, "name": "dep", "path": "third_party/dep", "sha": shaTip,
			"url": "https://api.github.com/x", "git_url": "https://api.github.com/y",
			"html_url": "https://github.com/z", "download_url": nil,
			"_links": map[string]any{"self": "https://api.github.com/x"},
		})
	}
	target := "/repos/org1/repo1/contents/third_party/dep?ref=main"
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes(), "submodule_git_url")
	assert.JSONEq(t, mustJSONString(map[string]any{
		"type": "submodule", "submodule_git_url": "https://github.com/org1/dep.git",
		"size": 0, "name": "dep", "path": "third_party/dep", "sha": shaTip,
	}), w1.Body.String())
	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
}

// The commits list answers a path filter from its own stored page, and an
// unknown ref from a stored verdict. A push to the ref drops both.
func TestCachedCommitsList_PathAndUnknownRef(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)
	base := u.list
	u.list = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sha") == "gone" {
			writeStatus(w, http.StatusNotFound, `{"message":"Not Found","status":"404"}`)
			return
		}
		base(w, r)
	}

	pathTarget := "/repos/org1/repo1/commits?sha=main&path=src/a.go&per_page=10"
	w1 := do(t, router, authedReq("GET", pathTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	w2 := do(t, router, authedReq("GET", pathTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())

	goneTarget := "/repos/org1/repo1/commits?sha=gone&per_page=10"
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", goneTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	require.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))

	for _, branch := range []string{"main", "gone"} {
		postWebhookJSON(t, router, "push", map[string]any{
			"ref": "refs/heads/" + branch, "before": shaBase, "after": shaTip,
			"repository": fixtureRepo(), "commits": []any{},
		})
	}
	assert.Equal(t, "miss", do(t, router, authedReq("GET", pathTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, http.StatusNotFound, do(t, router, authedReq("GET", goneTarget, nil)).Code)
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.listHits))
}
