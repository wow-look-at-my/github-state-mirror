package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// upstreamCommit builds a GitHub-shaped commits-LIST item (URL clutter,
// node_id, verification, and the top-level author/committer USER objects the
// rebuild must drop).
func upstreamCommit(sha, message, treeSHA string, parents ...string) map[string]any {
	api := "https://api.github.com/repos/org1/repo1/commits/" + sha
	parentObjs := make([]any, 0, len(parents))
	for _, p := range parents {
		parentObjs = append(parentObjs, map[string]any{
			"sha": p, "url": "https://api.github.com/repos/org1/repo1/commits/" + p,
			"html_url": "https://github.com/org1/repo1/commit/" + p,
		})
	}
	return map[string]any{
		"sha": sha, "node_id": "C_" + sha[:6],
		"url": api, "html_url": "https://github.com/org1/repo1/commit/" + sha,
		"comments_url": api + "/comments",
		"commit": map[string]any{
			"url":           api,
			"author":        map[string]any{"name": "Alice", "email": "alice@example.com", "date": "2026-07-01T10:00:00Z"},
			"committer":     map[string]any{"name": "Bob", "email": "bob@example.com", "date": "2026-07-01T10:05:00Z"},
			"message":       message,
			"tree":          map[string]any{"sha": treeSHA, "url": "https://api.github.com/repos/org1/repo1/git/trees/" + treeSHA},
			"comment_count": 0,
			"verification":  map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil},
		},
		"author": map[string]any{
			"login": "alice", "id": 1, "node_id": "U_1", "type": "User",
			"avatar_url": "https://avatars.github.com/alice", "url": "https://api.github.com/users/alice",
			"html_url": "https://github.com/alice",
		},
		"committer": map[string]any{
			"login": "bob", "id": 2, "node_id": "U_2", "type": "User",
			"avatar_url": "https://avatars.github.com/bob", "url": "https://api.github.com/users/bob",
			"html_url": "https://github.com/bob",
		},
		"parents": parentObjs,
	}
}

// commitsCacheUpstream is the fake GitHub for the commits-list cache tests.
type commitsCacheUpstream struct {
	listHits  int32
	probeHits int32
	list      func(w http.ResponseWriter, r *http.Request)
	probe     func(w http.ResponseWriter, r *http.Request)
}

func newCommitsCacheUpstream() *commitsCacheUpstream {
	u := &commitsCacheUpstream{}
	u.list = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("page") == "2":
			servePRJSON(w, []any{upstreamCommit(shaBase, "initial", shaTree1)})
		case q.Get("sha") == "dev":
			servePRJSON(w, []any{upstreamCommit(shaCommit, "dev tip", shaTree2, shaBase)})
		default: // newest first, like GitHub
			servePRJSON(w, []any{
				upstreamCommit(shaTip, "fix: a thing <with> & symbols", shaTree2, shaMid),
				upstreamCommit(shaMid, "feat: mid", shaTree1, shaBase),
			})
		}
	}
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		// Report a private repo so callers earn grants (like the other fakes).
		servePRJSON(w, map[string]any{
			"name": "repo1", "full_name": "org1/repo1", "private": true, "visibility": "private",
			"html_url": "https://github.com/org1/repo1", "default_branch": "main",
			"owner": map[string]any{"login": "org1"},
		})
	}
	return u
}

func (u *commitsCacheUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			servePRJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
		case strings.HasSuffix(r.URL.Path, "/commits"):
			atomic.AddInt32(&u.listHits, 1)
			u.list(w, r)
		case regexp.MustCompile(`^/repos/[^/]+/[^/]+$`).MatchString(r.URL.Path):
			atomic.AddInt32(&u.probeHits, 1)
			u.probe(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
		}
	})
}

func commitsCacheStack(t *testing.T) (http.Handler, *ghdata.Store, *sql.DB, *commitsCacheUpstream) {
	t.Helper()
	u := newCommitsCacheUpstream()
	router, store, db, _ := newTestStackWithGitHub(t, testAuth(), u.handler())
	return router, store, db, u
}

// TestCachedCommitsList_MissAbsorbHit covers the core flow: the first read
// fetches + absorbs (miss), the second serves the identical trimmed body from
// state (hit, zero upstream calls), the rebuild drops every URL field and the
// user-object clutter -- and the absorbed commits land in the SAME global
// git_commits_cache rows the single git-commit route serves.
func TestCachedCommitsList_MissAbsorbHit(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)
	target := "/repos/org1/repo1/commits?sha=main&per_page=100&page=1"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, fmt.Sprintf(`[
		{"sha": %q,
		 "commit": {
			"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-01T10:00:00Z"},
			"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-01T10:05:00Z"},
			"message": "fix: a thing <with> & symbols",
			"tree": {"sha": %q}},
		 "parents": [{"sha": %q}]},
		{"sha": %q,
		 "commit": {
			"author": {"name":"Alice","email":"alice@example.com","date":"2026-07-01T10:00:00Z"},
			"committer": {"name":"Bob","email":"bob@example.com","date":"2026-07-01T10:05:00Z"},
			"message": "feat: mid",
			"tree": {"sha": %q}},
		 "parents": [{"sha": %q}]}
	]`, shaTip, shaTree2, shaMid, shaMid, shaTree1, shaBase), w1.Body.String())

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits), "hit must not call upstream")

	// Absorb-don't-byte-cache: the listed commits are global git_commits_cache
	// rows, so the single git-commit route hits without its own fetch ever
	// having happened (this fake 404s that endpoint -- a miss could not serve).
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/git/commits/"+shaTip, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader), "list-absorbed commits must serve the single git-commit route")
}

// TestCachedCommitsList_PageAndRefKeying: every modeled query dimension is its
// own snapshot -- page=1 vs page=2, one ref vs another vs the bare default --
// and, unlike the pulls list, a page as long as per_page is still served from
// state (the snapshot IS that exact page's answer; the next page has its own
// key, so there is nothing to truncate).
func TestCachedCommitsList_PageAndRefKeying(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)

	w1 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=main&per_page=100&page=1", nil))
	require.Equal(t, "miss", w1.Header().Get(cacheHeader))
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=main&per_page=100&page=2", nil))
	require.Equal(t, "miss", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits), "each page fetches once")
	assert.NotEqual(t, w1.Body.String(), w2.Body.String())

	// Each page serves from its own snapshot (param order irrelevant).
	w1b := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?page=1&per_page=100&sha=main", nil))
	assert.Equal(t, "hit", w1b.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w1b.Body.String())
	w2b := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=main&per_page=100&page=2", nil))
	assert.Equal(t, "hit", w2b.Header().Get(cacheHeader))
	assert.Equal(t, w2.Body.String(), w2b.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))

	// A different ref is its own key; the bare default ('' ref) yet another.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=dev&per_page=100", nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	w4 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?per_page=100", nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader))
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.listHits))

	// A FULL page (len == per_page) is cached and served -- no pagination guard.
	w5 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=main&per_page=2", nil))
	assert.Equal(t, "miss", w5.Header().Get(cacheHeader))
	w6 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=main&per_page=2", nil))
	assert.Equal(t, "hit", w6.Header().Get(cacheHeader), "a full page is that key's exact answer and serves from state")
	assert.Equal(t, int32(5), atomic.LoadInt32(&u.listHits))
}

// TestCachedCommitsList_QueryShapeGuards: shapes the cache does not model pass
// through verbatim, uncached, every time -- and never poison the cache. The
// single-commit sub-path /commits/{sha} never matches the cached route either.
func TestCachedCommitsList_QueryShapeGuards(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/commits?path=src",                   // unmodeled filter
		"/repos/org1/repo1/commits?since=2026-01-01T00:00:00Z", // unmodeled filter
		"/repos/org1/repo1/commits?author=alice",               // unmodeled filter
		"/repos/org1/repo1/commits?sha=",                       // empty ref value
		"/repos/org1/repo1/commits?per_page=101",               // out of range
		"/repos/org1/repo1/commits?page=11",                    // beyond the modeled cap
		"/repos/org1/repo1/commits?sha=main&sha=dev",           // repeated param
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.listHits), target)
	}

	// A non-default Accept (the diff media type) passes through too.
	req := authedReq("GET", "/repos/org1/repo1/commits?sha=main", nil)
	req.Header.Set("Accept", "application/vnd.github.diff")
	w := do(t, router, req)
	assert.Empty(t, w.Header().Get(cacheHeader), "non-default Accept must pass through")
	assert.Equal(t, int32(8), atomic.LoadInt32(&u.listHits))

	// The single-commit endpoint is a DIFFERENT path and stays passthrough
	// (this fake does not serve it, so the proxied answer is its 404).
	wc := do(t, router, authedReq("GET", "/repos/org1/repo1/commits/"+shaCommit, nil))
	assert.Equal(t, http.StatusNotFound, wc.Code)
	assert.Empty(t, wc.Header().Get(cacheHeader), "/commits/{sha} must not hit the cached list route")
	assert.Equal(t, int32(8), atomic.LoadInt32(&u.listHits), "/commits/{sha} must not count as a list fetch")

	// Passthroughs stored nothing: a cacheable shape still misses.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/commits?sha=main", nil))
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader))
}

// TestCachedCommitsList_WebhookFlush: push AND repository events flush the
// repo's snapshots (a push moves every ref-relative listing) while the
// absorbed git-commit rows -- immutable -- survive.
func TestCachedCommitsList_WebhookFlush(t *testing.T) {
	router, _, db, u := commitsCacheStack(t)
	target := "/repos/org1/repo1/commits?sha=main&per_page=100"

	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	postWebhook(t, router, "push", `{"repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "push must flush the snapshot")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))

	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w3.Header().Get(cacheHeader), "the re-absorbed snapshot serves again")

	postWebhook(t, router, "repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"}}}`)
	w4 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "miss", w4.Header().Get(cacheHeader), "repository events must flush the snapshot too")
	assert.Equal(t, int32(3), atomic.LoadInt32(&u.listHits))

	// The absorbed commit rows are immutable global truth and survive flushes.
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM git_commits_cache`).Scan(&count))
	assert.Greater(t, count, 0, "git-commit rows must survive the snapshot flush")
}

// TestCachedCommitsList_TTLBackstopExpiry: even with webhooks silent, a
// snapshot expires after its TTL -- a missed push can't serve a stale listing
// forever.
func TestCachedCommitsList_TTLBackstopExpiry(t *testing.T) {
	router, _, db, u := commitsCacheStack(t)
	target := "/repos/org1/repo1/commits?sha=main"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	_, err := db.Exec(`UPDATE commits_list_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired snapshot is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestCachedCommitsList_MissingCommitRowMisses: a snapshot whose commit rows
// were pruned out of git_commits_cache degrades to a miss (never a hole), and
// the re-absorb self-heals.
func TestCachedCommitsList_MissingCommitRowMisses(t *testing.T) {
	router, _, db, u := commitsCacheStack(t)
	target := "/repos/org1/repo1/commits?sha=main&per_page=100"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	res, err := db.Exec(`DELETE FROM git_commits_cache WHERE sha = ?`, shaMid)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the seeded commit row must exist to be deleted")

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a snapshot with a pruned commit row must miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "the re-absorb restores the pair")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestCachedCommitsList_Non200NotStored: 404 (unknown ref), 409 (empty repo),
// 5xx -- anything but a 200 -- is relayed verbatim and stores no snapshot.
func TestCachedCommitsList_Non200NotStored(t *testing.T) {
	router, _, db, u := commitsCacheStack(t)
	u.list = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Git Repository is empty.","documentation_url":"https://docs.github.com"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/commits", nil))
		require.Equal(t, http.StatusConflict, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader), "a non-200 must be replayed unstored")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.listHits))
	}

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM commits_list_cache`).Scan(&count))
	assert.Zero(t, count, "a non-200 answer must store no snapshot")
}

// TestCachedCommitsList_RevealDenied: an unauthorized caller gets GitHub's own
// relayed denial and never reaches the commits fetch; the repeat request is
// answered from the deny cache without touching GitHub.
func TestCachedCommitsList_RevealDenied(t *testing.T) {
	router, _, _, u := commitsCacheStack(t)
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	}
	target := "/repos/org1/ghost/commits?sha=main"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader), "a fresh probe denial is a miss")
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.listHits), "a denied caller never reaches the commits fetch")

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusNotFound, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a cached deny verdict answers without GitHub")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.probeHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&u.listHits))
}

// TestParseCommitsListShape_Defaults pins the shape parser: the bare query is
// cacheable at GitHub's defaults, the pr-minder fork-point shape parses, and
// everything unmodeled is refused.
func TestParseCommitsListShape_Defaults(t *testing.T) {
	shape, ok := parseCommitsListShape(url.Values{})
	require.True(t, ok)
	assert.Equal(t, commitsDefaultPerPage, shape.perPage)
	assert.Equal(t, 1, shape.page)
	assert.Empty(t, shape.refParam)

	q, _ := url.ParseQuery("sha=claude/my-branch&per_page=100&page=2")
	shape, ok = parseCommitsListShape(q)
	require.True(t, ok)
	assert.Equal(t, "claude/my-branch", shape.refParam)
	assert.Equal(t, 100, shape.perPage)
	assert.Equal(t, 2, shape.page)

	for _, bad := range []string{
		"sha=", "per_page=0", "per_page=101", "page=0", "page=11",
		"path=src", "since=2026-01-01T00:00:00Z", "until=2026-01-02T00:00:00Z",
		"author=alice", "committer=bob", "first_parent=true", "sha=main&sha=dev",
	} {
		q, _ := url.ParseQuery(bad)
		_, ok := parseCommitsListShape(q)
		assert.False(t, ok, bad)
	}
}
