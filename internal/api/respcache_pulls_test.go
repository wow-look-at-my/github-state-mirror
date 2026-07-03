package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// upstreamPR builds a GitHub-shaped PR object (URL clutter and all) usable
// both as a fake /pulls list item / single-PR response and as a webhook
// payload's pull_request. Tests mutate the map before marshaling.
func upstreamPR(num int64, state, title, headRef, headSHA, createdAt string) map[string]any {
	api := fmt.Sprintf("https://api.github.com/repos/org1/repo1/pulls/%d", num)
	return map[string]any{
		"url": api, "id": num * 100, "node_id": fmt.Sprintf("PR_node%d", num),
		"html_url": fmt.Sprintf("https://github.com/org1/repo1/pull/%d", num),
		"diff_url": api + ".diff", "patch_url": api + ".patch", "issue_url": api,
		"number": num, "state": state, "locked": false,
		"title": title,
		"user": map[string]any{
			"login": "alice", "id": 1, "node_id": "U_1", "type": "User",
			"avatar_url": "https://avatars.github.com/alice", "url": "https://api.github.com/users/alice",
			"html_url": "https://github.com/alice",
		},
		"body":      "the description",
		"labels":    []any{},
		"milestone": nil, "active_lock_reason": nil,
		"created_at": createdAt, "updated_at": "2026-07-03T09:00:00Z",
		"closed_at": nil, "merged_at": nil,
		"merge_commit_sha": shaMid,
		"assignee":         nil, "assignees": []any{}, "requested_reviewers": []any{}, "requested_teams": []any{},
		"draft": false,
		"head": map[string]any{
			"label": "org1:" + headRef, "ref": headRef, "sha": headSHA,
			"user": map[string]any{"login": "org1", "url": "https://api.github.com/users/org1"},
			"repo": map[string]any{
				"id": 5, "name": "repo1", "full_name": "org1/repo1",
				"url": "https://api.github.com/repos/org1/repo1", "html_url": "https://github.com/org1/repo1",
			},
		},
		"base": map[string]any{
			"label": "org1:main", "ref": "main", "sha": shaBase,
			"repo": map[string]any{
				"id": 5, "name": "repo1", "full_name": "org1/repo1",
				"owner": map[string]any{"login": "org1", "url": "https://api.github.com/users/org1"},
				"url":   "https://api.github.com/repos/org1/repo1",
			},
		},
		"_links":             map[string]any{"self": map[string]any{"href": api}},
		"author_association": "MEMBER",
		"auto_merge":         nil,
	}
}

// withLabel attaches a GitHub-shaped label object to an upstreamPR map.
func withLabel(pr map[string]any, name, color string) map[string]any {
	pr["labels"] = append(pr["labels"].([]any), map[string]any{
		"id": 9, "node_id": "L_9", "url": "https://api.github.com/l/" + name,
		"name": name, "color": color, "default": false, "description": nil,
	})
	return pr
}

// prEvent marshals a pull_request webhook payload embedding the given PR.
func prEvent(action string, pr map[string]any) string {
	b, err := json.Marshal(map[string]any{
		"action":       action,
		"number":       pr["number"],
		"pull_request": pr,
		"repository":   map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// pullsCacheUpstream is the fake GitHub for the PR cache tests.
type pullsCacheUpstream struct {
	listHits    int32
	singleHits  int32
	installHits int32
	probeHits   int32
	list        func(w http.ResponseWriter, r *http.Request)
	single      func(w http.ResponseWriter, r *http.Request)
}

func servePRJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func newPullsCacheUpstream() *pullsCacheUpstream {
	u := &pullsCacheUpstream{}
	u.list = func(w http.ResponseWriter, r *http.Request) {
		// #8 is a draft with native auto-merge armed (enabled_by is a full
		// user object, URLs and all -- the rebuild must trim it to the
		// merge_method the consumers read).
		pr8 := upstreamPR(8, "open", "Second PR", "other-branch", shaTip, "2026-07-02T10:00:00Z")
		pr8["draft"] = true
		pr8["auto_merge"] = map[string]any{
			"enabled_by": map[string]any{
				"login": "alice", "id": 1, "url": "https://api.github.com/users/alice",
				"html_url": "https://github.com/alice", "avatar_url": "https://a",
			},
			"merge_method": "squash", "commit_title": nil, "commit_message": nil,
		}
		// GitHub's default order: newest created first.
		servePRJSON(w, []any{
			pr8,
			withLabel(upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z"), "auto-merge", "ededed"),
		})
	}
	u.single = func(w http.ResponseWriter, r *http.Request) {
		pr := upstreamPR(7, "open", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
		pr["mergeable"] = true
		pr["mergeable_state"] = "clean"
		pr["merged"] = false
		pr["additions"] = 10
		pr["deletions"] = 2
		servePRJSON(w, pr)
	}
	return u
}

func (u *pullsCacheUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
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
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 777, "slug": "testapp"})
		case strings.HasSuffix(r.URL.Path, "/installation"):
			atomic.AddInt32(&u.installHits, 1)
			servePRJSON(w, map[string]any{
				"id": 42,
				"account": map[string]any{
					"login": "org1", "id": 9000, "type": "Organization",
					"url": "https://api.github.com/orgs/org1", "avatar_url": "https://a", "html_url": "https://github.com/org1",
				},
				"repository_selection": "all",
				"access_tokens_url":    "https://api.github.com/app/installations/42/access_tokens",
				"repositories_url":     "https://api.github.com/installation/repositories",
				"html_url":             "https://github.com/organizations/org1/settings/installations/42",
				"app_id":               777, "app_slug": "testapp",
				"target_id": 9000, "target_type": "Organization",
				"permissions": map[string]any{"pull_requests": "write"},
				"events":      []any{"pull_request"},
			})
		case strings.Contains(r.URL.Path, "/pulls/"):
			atomic.AddInt32(&u.singleHits, 1)
			u.single(w, r)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			atomic.AddInt32(&u.listHits, 1)
			u.list(w, r)
		case regexp.MustCompile(`^/repos/[^/]+/[^/]+$`).MatchString(r.URL.Path):
			// The reveal probe: report a private repo so callers earn grants.
			atomic.AddInt32(&u.probeHits, 1)
			servePRJSON(w, map[string]any{
				"name": "repo1", "full_name": "org1/repo1", "private": true, "visibility": "private",
				"html_url": "https://github.com/org1/repo1", "default_branch": "main",
				"owner": map[string]any{"login": "org1"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com","status":"404"}`))
		}
	})
}

func pullsCacheStack(t *testing.T) (http.Handler, *ghdata.Store, *sql.DB, *pullsCacheUpstream) {
	t.Helper()
	u := newPullsCacheUpstream()
	router, store, db, _ := newTestStackWithGitHub(t, testAuth(), u.handler())
	return router, store, db, u
}

// TestCachedPullsList_MissAbsorbHit covers the core list flow: the first read
// fetches + absorbs (miss), the second serves the identical trimmed body from
// state (hit, zero upstream calls), and the rebuild drops every URL field.
func TestCachedPullsList_MissAbsorbHit(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100&page=1"

	w1 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))
	assertNoURLKeys(t, w1.Body.Bytes())

	var items []map[string]any
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &items))
	require.Len(t, items, 2)
	// Newest created first, and every field pr-minder reads is present.
	first, second := items[0], items[1]
	assert.Equal(t, float64(8), first["number"])
	assert.Equal(t, true, first["draft"])
	assert.Equal(t, map[string]any{"merge_method": "squash"}, first["auto_merge"],
		"an armed auto_merge must rebuild as a non-null object without URL clutter")
	assert.Equal(t, float64(7), second["number"])
	assert.Equal(t, "open", second["state"])
	assert.Equal(t, false, second["draft"])
	assert.Equal(t, "First PR", second["title"])
	assert.Equal(t, "the description", second["body"])
	assert.Equal(t, "PR_node7", second["node_id"])
	assert.Equal(t, map[string]any{"login": "alice", "type": "User"}, second["user"])
	assert.Equal(t, []any{map[string]any{"name": "auto-merge", "color": "ededed"}}, second["labels"])
	assert.Equal(t, shaMid, second["merge_commit_sha"])
	assert.Nil(t, second["auto_merge"])
	head := second["head"].(map[string]any)
	assert.Equal(t, "feature", head["ref"])
	assert.Equal(t, shaCommit, head["sha"])
	assert.Equal(t, map[string]any{"full_name": "org1/repo1"}, head["repo"])
	base := second["base"].(map[string]any)
	assert.Equal(t, "main", base["ref"])
	assert.Equal(t, shaBase, base["sha"])

	w2 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit must serve the same trimmed body as the miss")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits), "hit must not call upstream")

	// The bare default shape shares the same absorbed state.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls", nil))
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))
}

// TestCachedPullsList_WebhookMaintenance is the load-bearing maintenance test:
// the repo is never org-fetched -- global truth is seeded by the list absorb
// (plus the reveal probe's repository object) and maintained ENTIRELY by
// webhooks, which apply unconditionally under the global model. Open/close/
// label/synchronize events must all be reflected in subsequent list rebuilds
// with ZERO further upstream fetches.
func TestCachedPullsList_WebhookMaintenance(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100"

	// Absorb the complete list (PRs #7, #8). Nothing pre-seeded.
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "miss", w.Header().Get(cacheHeader))

	readNumbers := func() []float64 {
		t.Helper()
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "hit", w.Header().Get(cacheHeader), "maintained list must stay a hit: %s", w.Body.String())
		var items []map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
		var nums []float64
		for _, it := range items {
			nums = append(nums, it["number"].(float64))
		}
		return nums
	}

	// A pull_request opened event adds PR #9 to the maintained set.
	pr9 := upstreamPR(9, "open", "Third PR", "hotfix", shaTree1, "2026-07-03T10:00:00Z")
	postWebhook(t, router, "pull_request", prEvent("opened", pr9))
	assert.Equal(t, []float64{9, 8, 7}, readNumbers(), "opened webhook must add the PR")

	// A closed event removes PR #7.
	pr7 := upstreamPR(7, "closed", "First PR", "feature", shaCommit, "2026-07-01T10:00:00Z")
	postWebhook(t, router, "pull_request", prEvent("closed", pr7))
	assert.Equal(t, []float64{9, 8}, readNumbers(), "closed webhook must remove the PR")

	// A labeled event updates PR #8's labels in place.
	pr8 := withLabel(upstreamPR(8, "open", "Second PR", "other-branch", shaTip, "2026-07-02T10:00:00Z"), "urgent", "ff0000")
	postWebhook(t, router, "pull_request", prEvent("labeled", pr8))

	// A synchronize event moves PR #9's head sha.
	pr9sync := upstreamPR(9, "open", "Third PR", "hotfix", shaTree2, "2026-07-03T10:00:00Z")
	postWebhook(t, router, "pull_request", prEvent("synchronize", pr9sync))

	wFinal := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", wFinal.Header().Get(cacheHeader))
	var items []map[string]any
	require.NoError(t, json.Unmarshal(wFinal.Body.Bytes(), &items))
	require.Len(t, items, 2)
	assert.Equal(t, []any{map[string]any{"name": "urgent", "color": "ff0000"}}, items[1]["labels"], "labeled webhook must update labels")
	assert.Equal(t, shaTree2, items[0]["head"].(map[string]any)["sha"], "synchronize webhook must move the head sha")
	assertNoURLKeys(t, wFinal.Body.Bytes())

	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits),
		"webhook maintenance must never trigger an upstream fetch")
}

// TestCachedPullsList_GraphQLSyncInteraction: the GraphQL org sync merges into
// the same global rows. Its COALESCE upsert PRESERVES the REST-only columns on
// PRs it re-writes (the list stays a hit) -- but a PR the sync introduces that
// no REST source has seen yet is rest-incomplete, and the list must miss
// rather than rebuild a partial entry.
func TestCachedPullsList_GraphQLSyncInteraction(t *testing.T) {
	router, store, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100"

	do(t, router, authedReq("GET", target, nil))
	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, "hit", w.Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	graphqlPR := func(number int64, title string) dbgen.PullRequest {
		return dbgen.PullRequest{
			Owner: "org1", Repo: "repo1", Number: number, Title: title, Url: "u",
			State: "OPEN", CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
			Mergeable:   sql.NullString{String: "MERGEABLE", Valid: true},
			AuthorLogin: sql.NullString{String: "alice", Valid: true},
		}
	}

	// A sync re-writing the SAME open set (7, 8): REST columns are preserved
	// by the COALESCE upsert, so the list keeps hitting.
	now := time.Now()
	require.NoError(t, store.SyncOrgTruth(context.Background(), "org1", ghdata.OrgSyncData{
		Repos: []dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		PRsByRepo: map[string][]dbgen.PullRequest{
			"org1/repo1": {graphqlPR(7, "First PR"), graphqlPR(8, "Second PR")},
		},
	}, testUserActor, now, now))
	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader), "a GraphQL re-sync must not degrade rest-complete rows")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	// A sync introducing a NEVER-REST-SEEN PR (#99): rest-incomplete row in
	// the open set -> the list must miss and refetch.
	require.NoError(t, store.SyncOrgTruth(context.Background(), "org1", ghdata.OrgSyncData{
		Repos: []dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		PRsByRepo: map[string][]dbgen.PullRequest{
			"org1/repo1": {graphqlPR(7, "First PR"), graphqlPR(8, "Second PR"), graphqlPR(99, "Fresh from GraphQL")},
		},
	}, testUserActor, now, now))
	w3 := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader), "a rest-incomplete row in the set forces a refetch")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestCachedPullsList_PaginationFullGuard: a response as long as per_page may
// continue upstream, so it never sets the completeness marker; and a rebuilt
// set as long as the request's per_page is never served from state.
func TestCachedPullsList_PaginationFullGuard(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	// per_page=2 with a 2-item response: possibly truncated -> no marker.
	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=2", nil))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a full page must never claim completeness")
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.listHits))
	}

	// Absorb a complete set (2 items < per_page=100) -> marker set...
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=100", nil))
	require.Equal(t, "miss", w.Header().Get(cacheHeader))
	require.Equal(t, int32(3), atomic.LoadInt32(&u.listHits))
	// ...but a request whose per_page equals the rebuilt length still misses.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=2", nil))
	assert.Equal(t, "miss", w2.Header().Get(cacheHeader), "len(rebuilt) == per_page must miss")
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.listHits))
	// A roomier page is served from the marker-backed state.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=3", nil))
	assert.Equal(t, "hit", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(4), atomic.LoadInt32(&u.listHits))
}

// TestCachedPullsList_QueryShapeGuards: shapes the cache does not model pass
// through verbatim, uncached, every time -- and never poison the cache.
func TestCachedPullsList_QueryShapeGuards(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	for i, target := range []string{
		"/repos/org1/repo1/pulls?sort=updated",          // unknown param
		"/repos/org1/repo1/pulls?state=closed",          // non-open state
		"/repos/org1/repo1/pulls?state=all",             // non-open state
		"/repos/org1/repo1/pulls?page=2",                // beyond page 1
		"/repos/org1/repo1/pulls?per_page=200",          // out of range
		"/repos/org1/repo1/pulls?head=justabranch",      // head without owner:
		"/repos/org1/repo1/pulls?state=open&state=open", // repeated param
		"/repos/org1/repo1/pulls?base=main",             // unmodeled filter
	} {
		w := do(t, router, authedReq("GET", target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		assert.Empty(t, w.Header().Get(cacheHeader), "unmodeled shape must pass through: %s", target)
		assert.Equal(t, int32(i+1), atomic.LoadInt32(&u.listHits), target)
	}

	// Passthroughs must not have set the marker: a cacheable shape still misses.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls", nil))
	assert.Equal(t, "miss", w.Header().Get(cacheHeader))
}

// TestCachedPullsList_HeadFilter: the head=owner:branch shape is served from
// the marker-backed complete set -- a no-match answer is a cached empty array
// (the common case in pr-minder's branch sweeps), while a match as long as
// per_page falls to the pagination guard and misses.
func TestCachedPullsList_HeadFilter(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)

	// Absorb the complete set.
	do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?state=open&per_page=100", nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	// Filter matching PR #7, roomy per_page: served from state.
	w := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:feature&state=open", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	var items []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, float64(7), items[0]["number"])

	// No match: a cached empty array, even at per_page=1.
	w2 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:no-such-branch&state=open&per_page=1", nil))
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.JSONEq(t, `[]`, w2.Body.String())

	// A match at per_page=1 fills the page -> pagination guard -> miss.
	w3 := do(t, router, authedReq("GET", "/repos/org1/repo1/pulls?head=org1:feature&state=open&per_page=1", nil))
	assert.Equal(t, "miss", w3.Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestCachedPullsList_GlobalSharedViaReveal: the absorbed list is GLOBAL
// truth. A second user pays one reveal probe (their own token proving repo
// access) and then reads the same rebuilt list -- the list itself is fetched
// once, ever.
func TestCachedPullsList_GlobalSharedViaReveal(t *testing.T) {
	router, _, _, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100"

	do(t, router, authedReq("GET", target, nil)) // user A: probe + absorb

	reqB := httptest.NewRequest("GET", target, nil)
	reqB.Header.Set("Authorization", "Bearer other-token")
	w := do(t, router, reqB)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader), "global truth serves every granted principal")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits), "the list is fetched once, ever")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits), "each principal pays exactly one probe")

	w2 := do(t, router, authedReq("GET", target, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.probeHits), "grants are remembered; no re-probe")
}

// TestCachedPullsList_MarkerTTLBackstop: with webhooks silent, the marker
// expires and the next read refetches -- a missed delivery cannot serve a
// stale list forever.
func TestCachedPullsList_MarkerTTLBackstop(t *testing.T) {
	router, _, db, u := pullsCacheStack(t)
	target := "/repos/org1/repo1/pulls?state=open&per_page=100"

	do(t, router, authedReq("GET", target, nil))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.listHits))

	_, err := db.Exec(`UPDATE pulls_list_cache SET expires_at = '2000-01-01T00:00:00Z'`)
	require.NoError(t, err)

	w := do(t, router, authedReq("GET", target, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "miss", w.Header().Get(cacheHeader), "an expired marker is a miss")
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.listHits))
}

// TestParsePullsListShape_Defaults pins the shape parser's defaults: the bare
// query is cacheable at GitHub's default page size, explicit pr-minder shapes
// parse, and per_page bounds hold.
func TestParsePullsListShape_Defaults(t *testing.T) {
	shape, ok := parsePullsListShape(url.Values{})
	require.True(t, ok)
	assert.Equal(t, pullsDefaultPerPage, shape.perPage)
	assert.Empty(t, shape.head)

	q, _ := url.ParseQuery("state=open&per_page=100&page=1")
	shape, ok = parsePullsListShape(q)
	require.True(t, ok)
	assert.Equal(t, 100, shape.perPage)

	q, _ = url.ParseQuery("head=org1:feature&state=open&per_page=1")
	shape, ok = parsePullsListShape(q)
	require.True(t, ok)
	assert.Equal(t, 1, shape.perPage)
	assert.Equal(t, "org1:feature", shape.head)

	for _, bad := range []string{"per_page=0", "per_page=101", "page=0", "head=:x", "head=x:"} {
		q, _ := url.ParseQuery(bad)
		_, ok := parsePullsListShape(q)
		assert.False(t, ok, bad)
	}
}
