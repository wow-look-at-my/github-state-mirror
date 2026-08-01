package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Git-ref route tests (GET /repos/{owner}/{repo}/git/ref/{ref...}); the
// shared fake upstream (respCacheUpstream) lives in respcache_test.go.

// defaultGitRefUpstream answers with a GitHub-shaped ref object full of URL
// fields, so the tests can prove the rebuild drops them.
func defaultGitRefUpstream(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimPrefix(r.URL.Path, "/repos/org1/repo1/git/ref/")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{
		"ref": "refs/%s", "node_id": "REF_kwAE",
		"url": "https://api.github.com/repos/org1/repo1/git/refs/%s",
		"object": {"sha": %q, "type": "commit",
		           "url": "https://api.github.com/repos/org1/repo1/git/commits/%s"}
	}`, strings.TrimPrefix(ref, "refs/"), ref, shaTip, shaTip)
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
	assert.JSONEq(t, fmt.Sprintf(`{
		"ref": "refs/heads/main", "node_id": "REF_kwAE",
		"object": {"sha": %q, "type": "commit"}
	}`, shaTip), w1.Body.String())

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

// A push naming the ref flushes it — in EVERY spelling, since rows key the
// verbatim requested ref and callers may send any of the three.
func TestCachedGitRef_PushFlushesEverySpelling(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	spellings := []string{
		"/repos/org1/repo1/git/ref/heads/main",
		"/repos/org1/repo1/git/ref/refs/heads/main",
	}
	for _, target := range spellings {
		do(t, router, authedReq("GET", target, nil))
		require.Equal(t, "hit", do(t, router, authedReq("GET", target, nil)).Header().Get(cacheHeader))
	}
	before := atomic.LoadInt32(&u.gitRefHits)

	postWebhook(t, router, "push", `{"ref":"refs/heads/main","after":"`+shaTip+`",
		"repository":{"name":"repo1","owner":{"login":"org1"},"default_branch":"main"}}`)

	for _, target := range spellings {
		w := do(t, router, authedReq("GET", target, nil))
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "push must flush %s", target)
	}
	assert.Equal(t, before+2, atomic.LoadInt32(&u.gitRefHits))
}

// A push to a DIFFERENT branch must not flush this one: the whole point of
// keying per ref rather than per repo.
func TestCachedGitRef_OtherBranchPushKeepsHit(t *testing.T) {
	router, _, _, _ := respCacheStack(t)
	target := "/repos/org1/repo1/git/ref/heads/main"

	do(t, router, authedReq("GET", target, nil))
	postWebhook(t, router, "push", `{"ref":"refs/heads/other","after":"`+shaMid+`",
		"repository":{"name":"repo1","owner":{"login":"org1"},"default_branch":"main"}}`)

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
	postWebhook(t, router, "push", `{"ref":"refs/heads/gone","after":"`+shaTip+`",
		"repository":{"name":"repo1","owner":{"login":"org1"},"default_branch":"main"}}`)

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
