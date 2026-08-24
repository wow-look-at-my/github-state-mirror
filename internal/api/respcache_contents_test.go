package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The contents-route tests: both Accept shapes it models, the media types it
// refuses, its webhook flushes, and the reveal-layer cases that ride on the
// same fixture. The fake GitHub and the shared assertions live in
// respcache_fixture_test.go.

// TestCachedContents_FileHitAndPushInvalidation covers the core contents flow:
// a 200 file is absorbed on the first request (miss), the second request is
// served from state — same trimmed body, no upstream call, X-GSM-Cache: hit —
// and a push webhook for the repo flushes it so the next request refetches.
func TestCachedContents_FileHitAndPushInvalidation(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/contents/.github/cfg.jsonc"

	// Miss: fetched, absorbed, served rebuilt.
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))
	assertNoURLKeys(t, w1.Body.Bytes())

	var file map[string]interface{}
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &file))
	assert.Equal(t, "file", file["type"])
	assert.Equal(t, "base64", file["encoding"])
	assert.Equal(t, "aGVsbG8=\n", file["content"], "base64 content preserved exactly as GitHub sent it")
	assert.Equal(t, ".github/cfg.jsonc", file["path"])
	assert.Equal(t, float64(5), file["size"])

	// Hit: identical trimmed body, zero new upstream calls.
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same rebuilt body as the miss")
	assert.Equal(t, "application/json; charset=utf-8", w2.Header().Get("Content-Type"))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits), "hit must not call upstream")

	// Push webhook for the repo -> whole-repo contents flush -> refetch.
	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "push must invalidate the cached contents")
}

// TestCachedContents_404CachedAndInvalidated: the 404 "config file absent"
// answer is absorbed too (half the win for per-repo config probes), rebuilt as
// {"message":...,"status":"404"} without documentation_url, and flushed by the
// same push invalidation.
func TestCachedContents_404CachedAndInvalidated(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest","status":"404"}`))
	}
	target := "/repos/org1/repo1/contents/.github/config/pr-minder/pr-minder.jsonc"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{"message":"Not Found","status":"404"}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits), "the 404 must be served from cache")

	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w3.Code)
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "push must invalidate the cached 404")
}

// TestCachedContents_DirListing: a directory response is absorbed as trimmed
// entries and rebuilt as an array with every URL field dropped.
func TestCachedContents_DirListing(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`[
			{"type":"file","size":12,"name":"a.txt","path":"dir/a.txt","sha":"s1",
			 "url":"https://api.github.com/a","html_url":"https://github.com/a","git_url":"https://g","download_url":"https://d",
			 "_links":{"self":"https://api.github.com/a"}},
			{"type":"dir","size":0,"name":"sub","path":"dir/sub","sha":"s2",
			 "url":"https://api.github.com/b","html_url":"https://github.com/b","git_url":"https://g2","download_url":null,
			 "_links":{"self":"https://api.github.com/b"}}
		]`))
	}

	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/dir", nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `[
		{"type":"file","size":12,"name":"a.txt","path":"dir/a.txt","sha":"s1"},
		{"type":"dir","size":0,"name":"sub","path":"dir/sub","sha":"s2"}
	]`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/dir", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))
}

// TestCachedContents_QueryStringDistinct: the raw ref query is part of the
// cache key — ?ref=a and ?ref=b are separate entries, each hitting upstream
// once and each served from its own state afterwards.
func TestCachedContents_QueryStringDistinct(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "base64", "size": 1, "name": "f", "path": "f",
			"content": "ref=" + r.URL.Query().Get("ref"),
			"sha":     "s-" + r.URL.Query().Get("ref"),
		})
	}

	wa := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/f?ref=branch-a", nil))
	wb := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/f?ref=branch-b", nil))
	require.Equal(t, http.StatusOK, wa.Code)
	require.Equal(t, http.StatusOK, wb.Code)
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "distinct refs must fetch separately")
	assert.NotEqual(t, wa.Body.String(), wb.Body.String())

	wa2 := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/f?ref=branch-a", nil))
	wb2 := do(t, router, authedReq("GET", "/repos/org1/repo1/contents/f?ref=branch-b", nil))
	assert.Equal(t, "hit", wa2.Header().Get(cacheHeader))
	assert.Equal(t, "hit", wb2.Header().Get(cacheHeader))
	assert.Equal(t, wa.Body.String(), wa2.Body.String())
	assert.Equal(t, wb.Body.String(), wb2.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "both refs served from their own entries")
}

// TestCachedContents_GlobalTruthSharedViaReveal: ONE global truth store — a
// second user's read of the same private resource is answered from the state
// the first user's fetch absorbed. The second user still pays GitHub exactly
// one PROBE (their own token proving repo access, earning a grant); the
// contents themselves are never refetched.
func TestCachedContents_GlobalTruthSharedViaReveal(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/contents/secret.txt"

	w1 := do(t, router, authedReq("GET", target, nil)) // user A: probe + miss
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits), "user A's first touch probes")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))

	reqB := httptest.NewRequest("GET", target, nil)
	reqB.Header.Set("Authorization", "Bearer other-token")
	w2 := do(t, router, reqB) // user B: probe grants, then HITS shared truth
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "global truth serves every granted principal")
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits), "user B pays one probe, not a refetch")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits), "the contents are fetched once, ever")

	w3 := do(t, router, authedReq("GET", target, nil)) // user A again: grant cached, plain hit
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits), "grants are remembered; no re-probe")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))
}

// TestReveal_DenyVerdictCachedAuthoritativeOnly: a probe GitHub answers 404 is
// relayed as the caller's truth and remembered briefly (repeat requests are
// answered from the deny cache without touching GitHub); a TRANSIENT probe
// failure (500) is never cached — the next request probes again.
func TestReveal_DenyVerdictCachedAuthoritativeOnly(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	}
	target := "/repos/org1/ghost/contents/cfg.jsonc"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader), "a fresh probe denial is a miss")
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.contentsHits), "a denied caller never reaches the contents fetch")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a cached deny verdict answers without GitHub")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits), "the deny verdict absorbs the repeat probe")

	// Transient probe failures are NEVER cached as denials.
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	target2 := "/repos/org1/flaky/contents/cfg.jsonc"
	w3 := do(t, router, authedReq("GET", target2, nil))
	assert.Equal(t, http.StatusBadGateway, w3.Code, "a transient probe failure fails the request")
	w4 := do(t, router, authedReq("GET", target2, nil))
	assert.Equal(t, http.StatusBadGateway, w4.Code)
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.probeHits), "transient failures are retried, never cached as denials")
}

// TestReveal_PublicFastPath: once truth knows a repo is public (here via a
// repository webhook's payload), any principal reads its cached state with no
// probe at all.
func TestReveal_PublicFastPath(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// A webhook teaches truth the repo exists and is public.
	postWebhook(t, router, "repository", `{"action":"created","repository":{
		"name":"pub","full_name":"org1/pub","private":false,"visibility":"public",
		"html_url":"https://github.com/org1/pub","default_branch":"main",
		"owner":{"login":"org1"}}}`)

	target := "/repos/org1/pub/contents/readme.md"
	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.probeHits), "a public repo needs no probe")

	reqB := httptest.NewRequest("GET", target, nil)
	reqB.Header.Set("Authorization", "Bearer other-token")
	w2 := do(t, router, reqB)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "public truth serves any principal")
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))
}

// TestCachedContents_TTLBackstopExpiry: even without any webhook, a cached row
// expires after its TTL — a missed webhook can't serve stale state forever.
func TestCachedContents_TTLBackstopExpiry(t *testing.T) {
	router, _, db, u := respCacheStack(t)
	target := "/repos/org1/repo1/contents/cfg.jsonc"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))

	// Age the row past its TTL (simulating the backstop elapsing).
	_, err := db.Exec(`UPDATE contents_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired row is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "expiry must force a refetch")
}

// TestRespCache_RawAcceptServedFromCache: the raw file-body representation
// (Accept: application/vnd.github.raw — what simple-llm-ui's file-read tool
// sends) is modeled from the SAME row the default JSON representation is,
// decoded server-side from the absorbed base64 `content`. A miss on EITHER
// Accept variant satisfies both: this proves the shapes share one cache
// entry rather than needing two independent fetches.
func TestRespCache_RawAcceptServedFromCache(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/contents/.github/cfg.jsonc"
	rawReq := func() *http.Request {
		req := authedReq("GET", target, nil)
		req.Header.Set("Accept", "application/vnd.github.raw")
		return req
	}

	// Miss: the JSON probe absorbs the fixture's base64 "aGVsbG8=\n" (the
	// trailing newline is GitHub's MIME-style wrapping of the base64 TEXT,
	// not part of the decoded content -- "aGVsbG8=" alone decodes to the
	// 5-byte "hello", matching the fixture's own declared size:5), and the
	// raw-Accept caller is served those decoded bytes, not the JSON wrapper.
	w1 := do(t, router, rawReq())
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "hello", w1.Body.String(), "raw Accept serves the decoded file bytes")
	assert.Equal(t, "text/plain; charset=utf-8", w1.Header().Get("Content-Type"))
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits), "the miss probed upstream exactly once")

	// Hit: a second identical raw-Accept request costs no upstream call.
	w2 := do(t, router, rawReq())
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hello", w2.Body.String())
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits), "a raw-Accept hit must not call upstream")

	// The DEFAULT-Accept representation of the SAME path is ALSO a hit off
	// the row the raw-Accept miss just populated -- one absorb, both shapes.
	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits), "the default-Accept shape shares the raw miss's row")
	var file map[string]interface{}
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &file))
	assert.Equal(t, "aGVsbG8=\n", file["content"], "the JSON shape still serves the base64 GitHub sent")
}

// TestRespCache_RawAcceptMissProbesDefaultJSON verifies the upstream call a
// raw-Accept MISS makes carries the DEFAULT JSON Accept, not the caller's raw
// one -- go-github, PyGithub and octokit.js all resolve file-vs-directory the
// same way, since GitHub's behavior for raw Accept against a directory is
// undocumented (see cachedContents's file header).
func TestRespCache_RawAcceptMissProbesDefaultJSON(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	var gotAccept string
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "base64", "size": 5,
			"name": "f", "path": "f", "content": "aGVsbG8=\n", "sha": "s",
		})
	}
	req := authedReq("GET", "/repos/org1/repo1/contents/f", nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/vnd.github+json", gotAccept, "the probe fetch asks for the default JSON shape")
	assert.Equal(t, "hello", w.Body.String())
}

// TestRespCache_RawAcceptUnabsorbableFallsBackToPassthrough: a file too large
// for the base64 JSON form (GitHub answers such a probe with encoding:"none"
// and no content past ~1 MiB — docs.github.com/en/rest/repos/contents) cannot
// be served from this cache at all. A raw-Accept caller still gets a correct
// answer: a second, genuine passthrough carrying its OWN raw Accept header,
// never the unusable JSON probe body.
func TestRespCache_RawAcceptUnabsorbableFallsBackToPassthrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	rawBytes := []byte("the real raw bytes, straight from GitHub")
	var jsonProbeHits, rawFallbackHits int32
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.raw" {
			atomic.AddInt32(&rawFallbackHits, 1)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write(rawBytes)
			return
		}
		atomic.AddInt32(&jsonProbeHits, 1)
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "none", "size": 5 << 20,
			"name": "big.bin", "path": "big.bin", "sha": "s",
		})
	}
	req := authedReq("GET", "/repos/org1/repo1/contents/big.bin", nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w := do(t, router, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(rawBytes), w.Body.String(), "the caller's own raw Accept is answered directly")
	assert.Empty(t, w.Header().Get(cacheHeader), "never marked as cached")
	assert.Equal(t, int32(1), jsonProbeHits, "the JSON probe was tried once")
	assert.Equal(t, int32(1), rawFallbackHits, "and, failing to absorb, fell back to one real raw call")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "both calls reached upstream")
}

// TestRespCache_NonDefaultAcceptPassthrough: media types that change the
// response shape in ways this package does not model at all (html/object,
// or an ambiguous mixed Accept) are not modeled — the route must forward
// them verbatim, uncached. Raw (application/vnd.github.raw) is NOT one of
// these anymore -- see TestRespCache_RawAcceptServedFromCache.
func TestRespCache_NonDefaultAcceptPassthrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	raw := []byte("plain html bytes, not json")
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.html" {
			w.Header().Set("Content-Type", "application/vnd.github.html")
			_, _ = w.Write(raw)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","size":1,"name":"f","path":"f","content":"eA==","sha":"s"}`))
	}

	for i := 1; i <= 2; i++ {
		req := authedReq("GET", "/repos/org1/repo1/contents/f", nil)
		req.Header.Set("Accept", "application/vnd.github.html")
		w := do(t, router, req)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, string(raw), w.Body.String(), "an unmodeled media type must pass through untouched")
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.contentsHits), "non-default, non-raw Accept is never cached")
	}
}

// TestRespCache_RepositoryEventFlushesContents: repository events (rename /
// delete / visibility) flush the repo's contents state like a push does.
func TestRespCache_RepositoryEventFlushesContents(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/contents/cfg.jsonc"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.contentsHits))

	postWebhook(t, router, "repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.contentsHits), "repository event must flush contents state")
}
