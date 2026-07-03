package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Test object ids (full 40-hex, as GitHub uses).
const (
	shaBase   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaMid    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaTip    = "cccccccccccccccccccccccccccccccccccccccc"
	shaTree1  = "1111111111111111111111111111111111111111"
	shaTree2  = "2222222222222222222222222222222222222222"
	shaCommit = "dddddddddddddddddddddddddddddddddddddddd"
)

// goodAppJWT is the bearer the fake GitHub verifies as app id 777; any other
// bearer on GET /app is rejected, like the real endpoint.
const goodAppJWT = "good-app-jwt"

// respCacheUpstream is a fake GitHub for the cached-route tests: it stubs
// /user (requireAuth) and /app (App JWT verification) and counts + serves the
// cacheable endpoints, with GitHub-shaped bodies full of URL fields so the
// tests can prove the rebuilds drop them.
type respCacheUpstream struct {
	contentsHits int32
	commitHits   int32
	mintHits     int32
	probeHits    int32
	// contents answers GET /repos/... contents paths; settable per test.
	contents func(w http.ResponseWriter, r *http.Request)
	// probe answers the reveal probe (GET /repos/{owner}/{repo}); settable
	// per test. The default reports a PRIVATE repo, so callers earn grants.
	probe func(w http.ResponseWriter, r *http.Request)
	// tokenExpiry is the expires_at minted tokens carry.
	tokenExpiry time.Time
}

func newRespCacheUpstream() *respCacheUpstream {
	u := &respCacheUpstream{tokenExpiry: time.Now().Add(time.Hour)}
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{
			"name": %q, "full_name": %q, "private": true, "visibility": "private",
			"html_url": "https://github.com/%s", "default_branch": "main",
			"owner": {"login": %q, "avatar_url": "https://a", "html_url": "https://github.com/%s"}
		}`, parts[1], parts[0]+"/"+parts[1], parts[0]+"/"+parts[1], parts[0], parts[0])
	}
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		n := atomic.LoadInt32(&u.contentsHits)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{
			"type": "file", "encoding": "base64", "size": 5,
			"name": "cfg.jsonc", "path": ".github/cfg.jsonc",
			"content": "aGVsbG8=\n", "sha": %q,
			"url": "https://api.github.com/x", "git_url": "https://api.github.com/y",
			"html_url": "https://github.com/z", "download_url": "https://raw.github.com/w",
			"_links": {"self": "https://api.github.com/x"}
		}`, fmt.Sprintf("%040d", n))
	}
	return u
}

func (u *respCacheUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			// Per-user partitioning resolves every bearer token here (id AND
			// login required). Answer the shared test identity for testToken
			// and a DISTINCT user for any other token, so cross-credential
			// tests exercise two separate user scopes.
			if r.Header.Get("Authorization") == "Bearer "+testToken {
				_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"login": "otheruser", "id": testUserID + 1})
			}
		case r.URL.Path == "/app":
			if r.Header.Get("Authorization") != "Bearer "+goodAppJWT {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 777, "slug": "testapp"})
		case regexp.MustCompile(`^/repos/[^/]+/[^/]+$`).MatchString(r.URL.Path):
			// The reveal probe: is this repo visible to the caller's token?
			atomic.AddInt32(&u.probeHits, 1)
			u.probe(w, r)
		case strings.Contains(r.URL.Path, "/contents/"):
			atomic.AddInt32(&u.contentsHits, 1)
			u.contents(w, r)
		case strings.Contains(r.URL.Path, "/git/commits/"):
			atomic.AddInt32(&u.commitHits, 1)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprintf(w, `{
				"sha": %q, "node_id": "C_kwAE",
				"url": "https://api.github.com/repos/org1/repo1/git/commits/x",
				"html_url": "https://github.com/org1/repo1/commit/x",
				"author": {"name": "Alice", "email": "alice@example.com", "date": "2026-07-01T10:00:00Z"},
				"committer": {"name": "Bob", "email": "bob@example.com", "date": "2026-07-01T10:05:00Z"},
				"tree": {"sha": %q, "url": "https://api.github.com/trees/x"},
				"message": "fix: a thing <with> & symbols",
				"parents": [{"sha": %q, "url": "https://api.github.com/parent", "html_url": "https://github.com/parent"}],
				"verification": {"verified": false, "reason": "unsigned"}
			}`, shaCommit, shaTree1, shaBase)
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			n := atomic.AddInt32(&u.mintHits, 1)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{
				"token": "ghs_minted%d", "expires_at": %q,
				"permissions": {"contents": "read", "metadata": "read"},
				"repository_selection": "all"
			}`, n, u.tokenExpiry.UTC().Format(time.RFC3339))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
		}
	})
}

// respCacheStack builds the full router over the fake upstream, seeding
// nothing. Returns router, store, db, and the upstream.
func respCacheStack(t *testing.T) (http.Handler, *ghdata.Store, *sql.DB, *respCacheUpstream) {
	t.Helper()
	u := newRespCacheUpstream()
	router, store, db, _ := newTestStackWithGitHub(t, testAuth(), u.handler())
	return router, store, db, u
}

func do(t *testing.T, router http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// postWebhook delivers a signed webhook to the router.
func postWebhook(t *testing.T, router http.Handler, event, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, []byte(body)))
	w := do(t, router, req)
	require.Less(t, w.Code, 300, "webhook delivery must succeed: %s", w.Body.String())
}

// assertNoURLKeys walks a rebuilt JSON body recursively and fails on any key
// the trimmed contract bans: url, *_url, or _links.
func assertNoURLKeys(t *testing.T, body []byte) {
	t.Helper()
	var v interface{}
	require.NoError(t, json.Unmarshal(body, &v), "rebuilt body must be valid JSON: %s", body)
	var walk func(v interface{}, at string)
	walk = func(v interface{}, at string) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, val := range x {
				lk := strings.ToLower(k)
				assert.False(t, lk == "url" || strings.HasSuffix(lk, "_url") || lk == "_links",
					"rebuilt body must not contain URL key %q (at %s): %s", k, at, body)
				walk(val, at+"."+k)
			}
		case []interface{}:
			for i, val := range x {
				walk(val, fmt.Sprintf("%s[%d]", at, i))
			}
		}
	}
	walk(v, "$")
}

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
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","size":1,"name":"f","path":"f","content":%q,"sha":"s-%s"}`,
			"ref="+r.URL.Query().Get("ref"), r.URL.Query().Get("ref"))
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

// TestCachedGitCommit_HitImmuneToPush: a fetched git commit is absorbed,
// rebuilt trimmed (node_id/verification/urls dropped), served from state on
// the next read — and, being immutable, survives push events untouched.
func TestCachedGitCommit_HitImmuneToPush(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/repos/org1/repo1/git/commits/" + shaCommit

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"sha": %q,
		"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-01T10:00:00Z"},
		"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-01T10:05:00Z"},
		"message": "fix: a thing <with> & symbols",
		"tree": {"sha": %q},
		"parents": [{"sha": %q}]
	}`, shaCommit, shaTree1, shaBase), w1.Body.String())

	// Push events do NOT invalidate immutable commit state.
	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits), "immutable commit must survive push events")
}

// TestCachedGitCommit_AbsorbedFromPushWebhook: a push payload's commits are
// upserted into GLOBAL truth — for a repo nobody ever fetched — so the
// post-push read hits WITHOUT any GitHub fetch ever having happened, and
// rebuilds to the same trimmed shape a fetch-sourced row does. (The reader
// still pays the reveal probe: their own proof of repo access.)
func TestCachedGitCommit_AbsorbedFromPushWebhook(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// No seeding: the dispatcher applies to GLOBAL truth unconditionally —
	// this repo has never been fetched by anyone.
	push := fmt.Sprintf(`{
		"repository": {"name":"repo1","owner":{"login":"org1"}},
		"before": %q, "after": %q, "forced": false,
		"head_commit": {"timestamp": "2026-07-03T10:00:00Z"},
		"commits": [
			{"id": %q, "tree_id": %q, "message": "first", "timestamp": "2026-07-03T09:59:00Z",
			 "author": {"name":"Alice","email":"alice@example.com"},
			 "committer": {"name":"Bob","email":"bob@example.com"}},
			{"id": %q, "tree_id": %q, "message": "second", "timestamp": "2026-07-03T10:00:00Z",
			 "author": {"name":"Alice","email":"alice@example.com"},
			 "committer": {"name":"Bob","email":"bob@example.com"}}
		]
	}`, shaBase, shaTip, shaMid, shaTree1, shaTip, shaTree2)
	postWebhook(t, router, "push", push)

	// First commit: parent is the payload's `before`.
	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaMid, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "hit", w1.Header().Get(cacheHeader), "webhook-absorbed commit must be a hit")
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`{
		"sha": %q,
		"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-03T09:59:00Z"},
		"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-03T09:59:00Z"},
		"message": "first",
		"tree": {"sha": %q},
		"parents": [{"sha": %q}]
	}`, shaMid, shaTree1, shaBase), w1.Body.String())

	// Second commit: parent is its predecessor in the chain.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaTip, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	var tip struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &tip))
	require.Len(t, tip.Parents, 1)
	assert.Equal(t, shaMid, tip.Parents[0].SHA)
	assert.Equal(t, shaTree2, tip.Tree.SHA)

	assert.Equal(t, int32(0), atomic.LoadInt32(&u.commitHits),
		"push-absorbed commits must serve without ANY GitHub fetch ever having happened")
}

// TestCachedGitCommit_ForcedPushNotAbsorbed: a forced push's payload chain is
// untrustworthy (before is not the parent), so nothing is absorbed and the
// read falls back to a normal fetch-miss.
func TestCachedGitCommit_ForcedPushNotAbsorbed(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	push := fmt.Sprintf(`{
		"repository": {"name":"repo1","owner":{"login":"org1"}},
		"before": %q, "after": %q, "forced": true,
		"commits": [
			{"id": %q, "tree_id": %q, "message": "rewritten", "timestamp": "2026-07-03T10:00:00Z",
			 "author": {"name":"A","email":"a@x"}, "committer": {"name":"B","email":"b@x"}}
		]
	}`, shaBase, shaTip, shaTip, shaTree1)
	postWebhook(t, router, "push", push)

	w := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaTip, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "forced-push commits must not be absorbed")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.commitHits))
}

// TestCachedInstallToken_HitVariantsAndFlush covers the token-mint cache: the
// same app+installation+body serves from cache until expiry; a different body
// is a different token (its own mint); an installation event flushes.
func TestCachedInstallToken_HitVariantsAndFlush(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	target := "/app/installations/42/access_tokens"

	mint := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		return do(t, router, req)
	}

	// Miss: minted upstream, absorbed, rebuilt.
	w1 := mint("")
	require.Equal(t, http.StatusCreated, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	var m1 map[string]interface{}
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &m1))
	assert.Equal(t, "ghs_minted1", m1["token"])
	assert.Equal(t, "all", m1["repository_selection"])
	assert.Equal(t, map[string]interface{}{"contents": "read", "metadata": "read"}, m1["permissions"])

	// Hit: same app+installation+body -> the SAME minted token, no upstream call.
	w2 := mint("")
	require.Equal(t, http.StatusCreated, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.mintHits))

	// A different (permissions-subset) body is a DIFFERENT token: fresh mint.
	w3 := mint(`{"permissions":{"contents":"read"}}`)
	require.Equal(t, http.StatusCreated, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.mintHits), "a body variant must mint its own token")

	// ...and is cached under its own key.
	w4 := mint(`{"permissions": {"contents": "read"}}`) // same body modulo whitespace
	assert.Equal(t, "hit", w4.Header().Get(cacheHeader), "canonicalized bodies share a key")
	assert.Equal(t, w3.Body.String(), w4.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.mintHits))

	// installation event for id 42 -> flush -> next mint refetches.
	postWebhook(t, router, "installation", `{"action":"suspend","installation":{"id":42}}`)
	w5 := mint("")
	require.Equal(t, http.StatusCreated, w5.Code)
	assert.Equal(t, "miss", w5.Header().Get(cacheHeader), "installation event must flush cached mints")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.mintHits))
}

// TestCachedInstallToken_ExpiryBufferRemint: a token whose expires_at is
// inside the safety buffer is served but never cached, so every request
// re-mints — a cached mint always has usable lifetime left.
func TestCachedInstallToken_ExpiryBufferRemint(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.tokenExpiry = time.Now().Add(5 * time.Minute) // < 10-minute buffer

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+goodAppJWT)
		w := do(t, router, req)
		require.Equal(t, http.StatusCreated, w.Code)
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.mintHits), "a near-expiry token must re-mint every time")
	}
}

// TestCachedInstallToken_NonAppJWTPassthrough: a caller whose bearer does not
// verify as an App JWT is forwarded to GitHub unchanged and never cached.
func TestCachedInstallToken_NonAppJWTPassthrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest("POST", "/app/installations/42/access_tokens", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer not-an-app-jwt")
		w := do(t, router, req)
		require.Equal(t, http.StatusCreated, w.Code, "GitHub's own answer passes through")
		assert.Empty(t, w.Header().Get(cacheHeader), "passthrough responses carry no cache marker")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.mintHits), "unverified callers are never served from cache")
	}
}

// TestRespCache_PruneCap: each cache table is LRU-pruned on write, so it can
// never grow past the row cap.
func TestRespCache_PruneCap(t *testing.T) {
	prev := ghdata.CacheMaxRows
	ghdata.CacheMaxRows = 3
	t.Cleanup(func() { ghdata.CacheMaxRows = prev })

	router, _, db, _ := respCacheStack(t)
	for i := 0; i < 6; i++ {
		w := do(t, router, authedReq("GET", fmt.Sprintf("/repos/org1/repo1/contents/file-%d.txt", i), nil))
		require.Equal(t, http.StatusOK, w.Code)
	}

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM contents_cache`).Scan(&count))
	assert.LessOrEqual(t, count, 3, "prune-on-write must cap the table at CacheMaxRows")
	assert.Greater(t, count, 0)
}

// TestRespCache_NonDefaultAcceptPassthrough: media types that change the
// response shape (raw contents) are not modeled — the route must forward them
// verbatim, uncached, so a raw-accepting caller never gets a JSON rebuild.
func TestRespCache_NonDefaultAcceptPassthrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	raw := []byte("plain raw bytes, not json")
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.raw" {
			w.Header().Set("Content-Type", "application/vnd.github.raw")
			_, _ = w.Write(raw)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","size":1,"name":"f","path":"f","content":"eA==","sha":"s"}`))
	}

	for i := 1; i <= 2; i++ {
		req := authedReq("GET", "/repos/org1/repo1/contents/f", nil)
		req.Header.Set("Accept", "application/vnd.github.raw")
		w := do(t, router, req)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, string(raw), w.Body.String(), "raw representation must pass through untouched")
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.contentsHits), "non-default Accept is never cached")
	}
}

// TestCachedRoutes_RequestLogDispositions: the dashboard's request log must
// reflect the cached routes — a miss records `miss`, a repeat records `hit` —
// so the hit/miss counters finally show real numbers for REST traffic.
func TestCachedRoutes_RequestLogDispositions(t *testing.T) {
	svc := configuredAuth(t)
	u := newRespCacheUpstream()
	router, _, _, _ := newTestStackWithGitHub(t, svc, u.handler())

	target := "/repos/org1/repo1/contents/cfg.jsonc"
	for i := 0; i < 2; i++ { // miss, then hit
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code)
	}

	req := httptest.NewRequest("GET", "/api/requests", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)

	var snap requestLogSnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
	assert.GreaterOrEqual(t, snap.ByDisposition[DispMiss], int64(1), "first contents read records a miss")
	assert.GreaterOrEqual(t, snap.ByDisposition[DispHit], int64(1), "second contents read records a hit")

	var sawMiss, sawHit bool
	for _, e := range snap.Recent {
		if e.Path == target && e.Disposition == DispMiss {
			sawMiss = true
			assert.Equal(t, http.StatusOK, e.Status, "a miss records the upstream status")
		}
		if e.Path == target && e.Disposition == DispHit {
			sawHit = true
		}
	}
	assert.True(t, sawMiss && sawHit, "both dispositions must appear in the log")
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
