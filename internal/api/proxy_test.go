package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghjson"
)

func testAuth() *auth.Service {
	return auth.New(auth.Config{SessionKey: []byte("test-session-key")})
}

func assertURLStrippedJSON(t *testing.T, githubBody, gotBody string) {
	t.Helper()
	want, err := ghjson.StripURLFields([]byte(githubBody))
	require.NoError(t, err)
	assert.JSONEq(t, string(want), gotBody)
	assertNoURLFields(t, gotBody)
}

func assertNoURLFields(t *testing.T, body string) {
	t.Helper()
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, child := range x {
				assert.Falsef(t, ghjson.IsURLField(k), "URL field %q should be stripped", k)
				walk(child)
			}
		case []interface{}:
			for _, child := range x {
				walk(child)
			}
		}
	}
	var decoded interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &decoded))
	walk(decoded)
}

// TestProxy_ForwardsUnknownRESTPath verifies that a path the mirror does not
// cache is transparently forwarded to GitHub: method, path, query, token, and
// body are passed through, and the upstream status/body/headers come back. CORS
// headers are applied because chi runs Use middleware around the NotFound route.
func TestProxy_ForwardsUnknownRESTPath(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"resources":{"core":{"remaining":4999}}}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := authedReq("GET", "/rate_limit?foo=bar", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Upstream status is preserved; JSON body fields carrying GitHub URLs are stripped.
	assert.Equal(t, http.StatusTeapot, w.Code)
	assert.Contains(t, w.Body.String(), `"remaining":4999`)

	// Upstream received the forwarded request unchanged.
	assert.Equal(t, "GET", gotMethod)
	assert.Equal(t, "/rate_limit", gotPath)
	assert.Equal(t, "foo=bar", gotQuery)
	assert.Equal(t, "Bearer "+testToken, gotAuth, "caller's token must be forwarded")

	// Upstream response headers and CORS both pass through to the client.
	assert.Equal(t, "4999", w.Header().Get("X-RateLimit-Remaining"))
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

// TestProxy_DeduplicatesCORS verifies that when GitHub returns its own CORS
// headers (it does — Access-Control-Allow-Origin: *), the forwarded response
// carries exactly one Access-Control-Allow-Origin (the mirror's) so browsers do
// not reject it for having multiple values, while Expose-Headers is preserved.
func TestProxy_DeduplicatesCORS(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.Header().Set("Access-Control-Expose-Headers", "X-RateLimit-Remaining, Link")
		_, _ = w.Write([]byte(`{}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := authedReq("GET", "/rate_limit", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	acao := w.Header().Values("Access-Control-Allow-Origin")
	require.Len(t, acao, 1, "exactly one Access-Control-Allow-Origin (the mirror's)")
	assert.Equal(t, "*", acao[0])
	// GitHub's Expose-Headers must survive so clients can read X-RateLimit-* etc.
	assert.Equal(t, "X-RateLimit-Remaining, Link", w.Header().Get("Access-Control-Expose-Headers"))
}

// TestProxy_RequiresToken verifies the passthrough is not an open relay: a
// request without an Authorization header is rejected with 401 and never
// reaches GitHub.
func TestProxy_RequiresToken(t *testing.T) {
	var upstreamHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := httptest.NewRequest("GET", "/rate_limit", nil) // no token
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, int32(0), atomic.LoadInt32(&upstreamHits), "must not forward a tokenless request")
}

// TestProxy_Uncached verifies the passthrough does not cache: repeated requests
// each reach GitHub.
func TestProxy_Uncached(t *testing.T) {
	var upstreamHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	for i := 0; i < 3; i++ {
		req := authedReq("GET", "/repos/o/r/releases", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&upstreamHits), "each call must reach GitHub uncached")
}

// TestProxy_AppInstallationAccessTokensPassthrough verifies POST
// /app/installations/{id}/access_tokens stays uncached passthrough. This endpoint
// mints a short-lived credential, so storing or replaying the response would be a
// security bug; the mirror must forward the caller's auth and request body to
// GitHub every time.
func TestProxy_AppInstallationAccessTokensPassthrough(t *testing.T) {
	var upstreamHits int32
	var gotMethods []string
	var gotPaths []string
	var gotAuths []string
	var gotBodies []string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		gotMethods = append(gotMethods, r.Method)
		gotPaths = append(gotPaths, r.URL.Path)
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		gotBodies = append(gotBodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"synthetic-installation-token","expires_at":"2026-06-21T01:00:00Z"}`)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	const body = `{"repositories":["github-state-mirror"],"permissions":{"contents":"read","pull_requests":"write"}}`
	for i := 0; i < 2; i++ {
		req := authedReq("POST", "/app/installations/123/access_tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "synthetic-installation-token",
			"response must come from upstream, not a cached credential")
	}

	assert.Equal(t, int32(2), atomic.LoadInt32(&upstreamHits), "each token mint must reach GitHub uncached")
	assert.Equal(t, []string{"POST", "POST"}, gotMethods)
	assert.Equal(t, []string{"/app/installations/123/access_tokens", "/app/installations/123/access_tokens"}, gotPaths)
	assert.Equal(t, []string{"Bearer " + testToken, "Bearer " + testToken}, gotAuths,
		"caller's token must be forwarded")
	assert.Equal(t, []string{body, body}, gotBodies, "request body must be forwarded verbatim")
}

// TestProxy_CompareRoutePassthrough verifies GET
// /repos/{owner}/{repo}/compare/{base}...{head} is not a cached REST route. The
// comparison body is large and variable, and the old cached route returned only
// a trimmed subset; it must be forwarded every time, with GitHub URL fields stripped.
func TestProxy_CompareRoutePassthrough(t *testing.T) {
	const body = `{"url":"https://api.github.test/repos/o/r/compare/main...feat","html_url":"https://github.test/o/r/compare/main...feat","permalink_url":"https://github.test/o/r/compare/abc...def","diff_url":"https://github.test/o/r/compare/main...feat.diff","patch_url":"https://github.test/o/r/compare/main...feat.patch","base_commit":{"sha":"abc","commit":{"message":"base"}},"merge_base_commit":{"sha":"abc"},"status":"ahead","ahead_by":2,"behind_by":0,"total_commits":2,"commits":[{"sha":"def","parents":[{"sha":"abc"}]}],"files":[{"filename":"a.go","status":"modified","additions":1,"deletions":0,"changes":1,"blob_url":"https://github.test/o/r/blob/def/a.go","raw_url":"https://github.test/o/r/raw/def/a.go","contents_url":"https://api.github.test/repos/o/r/contents/a.go?ref=def","patch":"@@ -1 +1 @@"}]}`
	var upstreamHits int32
	var gotPaths []string
	var gotQueries []string
	var gotAuths []string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		gotPaths = append(gotPaths, r.URL.Path)
		gotQueries = append(gotQueries, r.URL.RawQuery)
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	router, store, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	for i := 0; i < 2; i++ {
		req := authedReq("GET", "/repos/o/r/compare/main...feat?per_page=100", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assertURLStrippedJSON(t, body, w.Body.String())
	}

	assert.Equal(t, int32(2), atomic.LoadInt32(&upstreamHits), "each compare call must reach GitHub uncached")
	assert.Equal(t, []string{"/repos/o/r/compare/main...feat", "/repos/o/r/compare/main...feat"}, gotPaths)
	assert.Equal(t, []string{"per_page=100", "per_page=100"}, gotQueries)
	assert.Equal(t, []string{"Bearer " + testToken, "Bearer " + testToken}, gotAuths,
		"caller's token must be forwarded")

	_, err := store.GetComparison(seedCtx(), "o", "r", "main", "feat")
	assert.ErrorIs(t, err, sql.ErrNoRows, "passthrough compare must not populate the comparison cache")
}

// TestProxy_SearchIssuesPassthrough verifies GitHub issue search stays
// passthrough. Search results include GitHub-computed ranking/scoring and are
// not webhook-feedable, so the mirror must forward the query and caller token
// instead of serving from cache.
func TestProxy_SearchIssuesPassthrough(t *testing.T) {
	const body = `{"total_count":1,"incomplete_results":false,"items":[{"url":"https://api.github.test/repos/o/r/issues/1","repository_url":"https://api.github.test/repos/o/r","labels_url":"https://api.github.test/repos/o/r/issues/1/labels{/name}","score":1}],"search_type":"lexical"}`
	var upstreamHits int32
	var gotPaths []string
	var gotQueries []string
	var gotAuths []string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		gotPaths = append(gotPaths, r.URL.Path)
		gotQueries = append(gotQueries, r.URL.RawQuery)
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	target := "/search/issues?q=repo%3Ao%2Fr+is%3Aissue+bug&sort=updated&order=desc&per_page=1"
	for i := 0; i < 2; i++ {
		req := authedReq("GET", target, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assertURLStrippedJSON(t, body, w.Body.String())
	}

	assert.Equal(t, int32(2), atomic.LoadInt32(&upstreamHits), "each issue search call must reach GitHub uncached")
	assert.Equal(t, []string{"/search/issues", "/search/issues"}, gotPaths)
	assert.Equal(t, []string{
		"q=repo%3Ao%2Fr+is%3Aissue+bug&sort=updated&order=desc&per_page=1",
		"q=repo%3Ao%2Fr+is%3Aissue+bug&sort=updated&order=desc&per_page=1",
	}, gotQueries)
	assert.Equal(t, []string{"Bearer " + testToken, "Bearer " + testToken}, gotAuths,
		"caller's token must be forwarded")
}

// TestProxy_MethodNotAllowedForwarded verifies that a known path hit with an
// unregistered method (e.g. POST /user, which only exists as GET) is forwarded
// rather than 405'd, so the mirror is a complete GitHub surface.
func TestProxy_MethodNotAllowedForwarded(t *testing.T) {
	var gotMethod, gotPath string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := authedReq("POST", "/user", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "POST", gotMethod)
	assert.Equal(t, "/user", gotPath)
}

// TestProxy_PreflightNotForwarded verifies a CORS preflight on an unknown path
// is answered locally (204) and never forwarded to GitHub.
func TestProxy_PreflightNotForwarded(t *testing.T) {
	var upstreamHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	req := httptest.NewRequest(http.MethodOptions, "/rate_limit", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, int32(0), atomic.LoadInt32(&upstreamHits), "preflight must not be forwarded")
}

// TestProxy_UserEndpointsPassThrough verifies /user and /user/orgs stay
// uncached passthroughs. Both responses carry credential-specific/full GitHub
// fields that the old cache shape did not store; /user/orgs also preserves query
// parameters. URL-bearing pagination headers are stripped with the body URL fields.
func TestProxy_UserEndpointsPassThrough(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		wantPath  string
		wantQuery string
		body      string
		link      string
	}{
		{
			name:     "user",
			target:   "/user",
			wantPath: "/user",
			body:     `{"login":"octocat","id":1,"node_id":"MDQ6VXNlcjE=","avatar_url":"https://avatars.githubusercontent.com/u/1?v=4","gravatar_id":"","url":"https://api.github.com/users/octocat","html_url":"https://github.com/octocat","followers_url":"https://api.github.com/users/octocat/followers","following_url":"https://api.github.com/users/octocat/following{/other_user}","gists_url":"https://api.github.com/users/octocat/gists{/gist_id}","starred_url":"https://api.github.com/users/octocat/starred{/owner}{/repo}","subscriptions_url":"https://api.github.com/users/octocat/subscriptions","organizations_url":"https://api.github.com/users/octocat/orgs","repos_url":"https://api.github.com/users/octocat/repos","events_url":"https://api.github.com/users/octocat/events{/privacy}","received_events_url":"https://api.github.com/users/octocat/received_events","type":"User","site_admin":false,"name":"The Octocat","company":"GitHub","blog":"https://github.blog","location":"San Francisco","email":"octocat@example.com","hireable":null,"bio":"There is no cache shape here","twitter_username":"octocat","public_repos":8,"public_gists":8,"followers":9999,"following":9,"created_at":"2011-01-25T18:44:36Z","updated_at":"2026-06-21T00:00:00Z","private_gists":1,"total_private_repos":2,"owned_private_repos":2,"disk_usage":42,"collaborators":3,"two_factor_authentication":true,"plan":{"name":"pro","space":976562499,"collaborators":0,"private_repos":9999}}`,
		},
		{
			name:      "user-orgs",
			target:    "/user/orgs?per_page=1&page=2",
			wantPath:  "/user/orgs",
			wantQuery: "per_page=1&page=2",
			body:      `[{"login":"github","id":9919,"node_id":"MDEyOk9yZ2FuaXphdGlvbjk5MTk=","url":"https://api.github.com/orgs/github","repos_url":"https://api.github.com/orgs/github/repos","events_url":"https://api.github.com/orgs/github/events","hooks_url":"https://api.github.com/orgs/github/hooks","issues_url":"https://api.github.com/orgs/github/issues","members_url":"https://api.github.com/orgs/github/members{/member}","public_members_url":"https://api.github.com/orgs/github/public_members{/member}","avatar_url":"https://avatars.githubusercontent.com/u/9919?v=4","description":"How people build software"}]`,
			link:      `<https://api.github.com/user/orgs?per_page=1&page=3>; rel="next", <https://api.github.com/user/orgs?per_page=1&page=4>; rel="last"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			var gotPath, gotQuery, gotAuth string
			gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				gotAuth = r.Header.Get("Authorization")
				if tc.link != "" {
					w.Header().Set("Link", tc.link)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})
			router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

			for i := 0; i < 2; i++ {
				req := authedReq("GET", tc.target, nil)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusOK, w.Code)
				assertURLStrippedJSON(t, tc.body, w.Body.String())
				if tc.link != "" {
					assert.Empty(t, w.Header().Get("Link"), "GitHub URL-bearing Link headers should be stripped")
				}
			}

			assert.Equal(t, int32(2), atomic.LoadInt32(&hits),
				"each request must reach GitHub uncached")
			assert.Equal(t, tc.wantPath, gotPath)
			assert.Equal(t, tc.wantQuery, gotQuery)
			assert.Equal(t, "Bearer "+testToken, gotAuth)
		})
	}
}

// TestProxy_FormerlyCachedNowForwarded verifies the endpoints the mirror used to
// cache with TRIMMED shapes (/user, /compare, /pulls/{n}/files) now pass through
// to GitHub uncached, with URL fields stripped from JSON responses.
func TestProxy_FormerlyCachedNowForwarded(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"user", "/user", `{"login":"octocat","id":1,"node_id":"MDQ6VXNlcjE=","type":"User","site_admin":false}`},
		{"compare", "/repos/o/r/compare/main...feat", `{"ahead_by":2,"behind_by":0,"total_commits":2,"files":[{"filename":"a.go","patch":"@@ -1 +1 @@"}]}`},
		{"pr-files", "/repos/o/r/pulls/5/files", `[{"filename":"a.go","status":"modified","additions":1,"deletions":0,"patch":"@@ -1 +1 @@"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})
			router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

			req := authedReq("GET", tc.path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			assertURLStrippedJSON(t, tc.body, w.Body.String())
			assert.Equal(t, int32(1), atomic.LoadInt32(&hits),
				"request must reach GitHub (no longer served from a trimmed cache)")
		})
	}
}

// TestRepoPullListRawCache verifies GET /repos/{owner}/{repo}/pulls stores and
// replays GitHub's normalized PR-list page for the exact query/media type key.
// The structured PR table is deliberately not used for this endpoint.
func TestRepoPullListRawCache(t *testing.T) {
	const body = `[{"url":"https://api.github.com/repos/o/r/pulls/42","id":4200,"node_id":"PR_kwDO","html_url":"https://github.com/o/r/pull/42","diff_url":"https://github.com/o/r/pull/42.diff","patch_url":"https://github.com/o/r/pull/42.patch","issue_url":"https://api.github.com/repos/o/r/issues/42","number":42,"state":"open","locked":false,"title":"Full list shape","user":{"login":"octocat","id":1,"node_id":"MDQ6VXNlcjE=","avatar_url":"https://avatars/u/1","gravatar_id":"","url":"https://api.github.com/users/octocat","html_url":"https://github.com/octocat","followers_url":"https://api.github.com/users/octocat/followers","following_url":"https://api.github.com/users/octocat/following{/other_user}","gists_url":"https://api.github.com/users/octocat/gists{/gist_id}","starred_url":"https://api.github.com/users/octocat/starred{/owner}{/repo}","subscriptions_url":"https://api.github.com/users/octocat/subscriptions","organizations_url":"https://api.github.com/users/octocat/orgs","repos_url":"https://api.github.com/users/octocat/repos","events_url":"https://api.github.com/users/octocat/events{/privacy}","received_events_url":"https://api.github.com/users/octocat/received_events","type":"User","user_view_type":"public","site_admin":false},"body":"hello","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-20T13:45:25Z","closed_at":null,"merged_at":null,"merge_commit_sha":"abc","assignees":[],"requested_reviewers":[],"requested_teams":[],"labels":[{"id":9,"node_id":"LA_kwDO","url":"https://api.github.com/repos/o/r/labels/bug","name":"bug","color":"d73a4a","default":true,"description":"Something is not working"}],"milestone":null,"draft":false,"commits_url":"https://api.github.com/repos/o/r/pulls/42/commits","review_comments_url":"https://api.github.com/repos/o/r/pulls/42/comments","review_comment_url":"https://api.github.com/repos/o/r/pulls/comments{/number}","comments_url":"https://api.github.com/repos/o/r/issues/42/comments","statuses_url":"https://api.github.com/repos/o/r/statuses/abc","head":{"label":"o:feature","ref":"feature","sha":"abc","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"base":{"label":"o:main","ref":"main","sha":"def","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"_links":{"self":{"href":"https://api.github.com/repos/o/r/pulls/42"},"html":{"href":"https://github.com/o/r/pull/42"},"issue":{"href":"https://api.github.com/repos/o/r/issues/42"},"comments":{"href":"https://api.github.com/repos/o/r/issues/42/comments"},"review_comments":{"href":"https://api.github.com/repos/o/r/pulls/42/comments"},"review_comment":{"href":"https://api.github.com/repos/o/r/pulls/comments{/number}"},"commits":{"href":"https://api.github.com/repos/o/r/pulls/42/commits"},"statuses":{"href":"https://api.github.com/repos/o/r/statuses/abc"}},"author_association":"MEMBER","auto_merge":null,"assignee":null,"active_lock_reason":null},{"url":"https://api.github.com/repos/o/r/pulls/41","id":4100,"node_id":"PR_kwDP","html_url":"https://github.com/o/r/pull/41","diff_url":"https://github.com/o/r/pull/41.diff","patch_url":"https://github.com/o/r/pull/41.patch","issue_url":"https://api.github.com/repos/o/r/issues/41","number":41,"state":"open","locked":false,"title":"Second PR","user":{"login":"hubot","id":2,"node_id":"MDQ6VXNlcjI=","avatar_url":"https://avatars/u/2","gravatar_id":"","url":"https://api.github.com/users/hubot","html_url":"https://github.com/hubot","followers_url":"https://api.github.com/users/hubot/followers","following_url":"https://api.github.com/users/hubot/following{/other_user}","gists_url":"https://api.github.com/users/hubot/gists{/gist_id}","starred_url":"https://api.github.com/users/hubot/starred{/owner}{/repo}","subscriptions_url":"https://api.github.com/users/hubot/subscriptions","organizations_url":"https://api.github.com/users/hubot/orgs","repos_url":"https://api.github.com/users/hubot/repos","events_url":"https://api.github.com/users/hubot/events{/privacy}","received_events_url":"https://api.github.com/users/hubot/received_events","type":"Bot","user_view_type":"public","site_admin":false},"body":null,"created_at":"2026-06-02T00:00:00Z","updated_at":"2026-06-20T11:11:15Z","closed_at":null,"merged_at":null,"merge_commit_sha":"123","assignees":[],"requested_reviewers":[],"requested_teams":[],"labels":[],"milestone":null,"draft":true,"commits_url":"https://api.github.com/repos/o/r/pulls/41/commits","review_comments_url":"https://api.github.com/repos/o/r/pulls/41/comments","review_comment_url":"https://api.github.com/repos/o/r/pulls/comments{/number}","comments_url":"https://api.github.com/repos/o/r/issues/41/comments","statuses_url":"https://api.github.com/repos/o/r/statuses/123","head":{"label":"o:feature-2","ref":"feature-2","sha":"123","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"base":{"label":"o:main","ref":"main","sha":"def","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"_links":{"self":{"href":"https://api.github.com/repos/o/r/pulls/41"},"html":{"href":"https://github.com/o/r/pull/41"},"issue":{"href":"https://api.github.com/repos/o/r/issues/41"},"comments":{"href":"https://api.github.com/repos/o/r/issues/41/comments"},"review_comments":{"href":"https://api.github.com/repos/o/r/pulls/41/comments"},"review_comment":{"href":"https://api.github.com/repos/o/r/pulls/comments{/number}"},"commits":{"href":"https://api.github.com/repos/o/r/pulls/41/commits"},"statuses":{"href":"https://api.github.com/repos/o/r/statuses/123"}},"author_association":"CONTRIBUTOR","auto_merge":null,"assignee":null,"active_lock_reason":null}]`
	const link = `<https://api.github.com/repos/o/r/pulls?state=open&per_page=2&page=3>; rel="next", <https://api.github.com/repos/o/r/pulls?state=open&per_page=2&page=9>; rel="last"`
	body2 := strings.Replace(body, "Full list shape", "Full list shape v2", 1)

	var authHits int32
	var listHits int32
	var gotPaths []string
	var gotQueries []string
	var gotAuths []string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			atomic.AddInt32(&authHits, 1)
			_, _ = io.WriteString(w, `{"login":"testuser"}`)
			return
		}
		hit := atomic.AddInt32(&listHits, 1)
		gotPaths = append(gotPaths, r.URL.Path)
		gotQueries = append(gotQueries, r.URL.RawQuery)
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", link)
		w.Header().Set("ETag", `W/"pull-list-page-2"`)
		if hit == 1 {
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, body2)
	})
	router, store, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	for i := 0; i < 2; i++ {
		req := authedReq("GET", "/repos/o/r/pulls?state=open&sort=updated&direction=desc&per_page=2&page=2", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assertURLStrippedJSON(t, body, w.Body.String())
		assert.Empty(t, w.Header().Get("Link"), "cached response should not preserve URL-bearing pagination links")
		assert.Empty(t, w.Header().Get("ETag"), "cached response should not preserve validators for rewritten bodies")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&authHits), "requireAuth should validate the token once")
	assert.Equal(t, int32(1), atomic.LoadInt32(&listHits), "second list call should be served from normalized cache")
	assert.Equal(t, []string{"/repos/o/r/pulls"}, gotPaths)
	assert.Equal(t, []string{
		"state=open&sort=updated&direction=desc&per_page=2&page=2",
	}, gotQueries)
	assert.Equal(t, []string{"Bearer " + testToken}, gotAuths,
		"caller's token must be forwarded")

	_, err := store.GetPullRequest(seedCtx(), "o", "r", 42)
	assert.ErrorIs(t, err, sql.ErrNoRows, "raw PR-list cache must not populate the structured PR cache")

	webhookBody := []byte(`{"action":"synchronize","pull_request":{"number":42,"title":"Full list shape v2","html_url":"https://github.com/o/r/pull/42","draft":false,"state":"open","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-20T14:00:00Z","head":{"ref":"feature","sha":"abc"},"base":{"ref":"main","repo":{"name":"r","owner":{"login":"o"}}},"labels":[]},"repository":{"name":"r","owner":{"login":"o"}}}`)
	hook := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(webhookBody))
	hook.Header.Set("X-GitHub-Event", "pull_request")
	hook.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, webhookBody))
	hookResp := httptest.NewRecorder()
	router.ServeHTTP(hookResp, hook)
	require.Equal(t, http.StatusAccepted, hookResp.Code)

	req := authedReq("GET", "/repos/o/r/pulls?state=open&sort=updated&direction=desc&per_page=2&page=2", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assertURLStrippedJSON(t, body2, w.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&listHits))
}

// TestPullRequestRawCache verifies GET /repos/{owner}/{repo}/pulls/{number}
// stores and replays GitHub's normalized response body, then refetches after a
// pull_request webhook marks that REST resource stale.
func TestPullRequestRawCache(t *testing.T) {
	const body1 = `{"url":"https://api.github.com/repos/o/r/pulls/5","id":123,"node_id":"PR_kwDO","html_url":"https://github.com/o/r/pull/5","diff_url":"https://github.com/o/r/pull/5.diff","patch_url":"https://github.com/o/r/pull/5.patch","issue_url":"https://api.github.com/repos/o/r/issues/5","number":5,"state":"open","locked":false,"title":"Full GitHub shape v1","user":{"login":"octocat","id":1,"node_id":"MDQ6VXNlcjE=","avatar_url":"https://avatars/u/1","html_url":"https://github.com/octocat","type":"User","site_admin":false},"body":"hello","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-02T00:00:00Z","closed_at":null,"merged_at":null,"merge_commit_sha":"abc","assignee":null,"assignees":[],"requested_reviewers":[],"requested_teams":[],"labels":[],"milestone":null,"draft":false,"commits_url":"https://api.github.com/repos/o/r/pulls/5/commits","review_comments_url":"https://api.github.com/repos/o/r/pulls/5/comments","review_comment_url":"https://api.github.com/repos/o/r/pulls/comments{/number}","comments_url":"https://api.github.com/repos/o/r/issues/5/comments","statuses_url":"https://api.github.com/repos/o/r/statuses/abc","head":{"label":"o:feature","ref":"feature","sha":"abc","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"base":{"label":"o:main","ref":"main","sha":"def","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"_links":{"self":{"href":"https://api.github.com/repos/o/r/pulls/5"},"html":{"href":"https://github.com/o/r/pull/5"}},"author_association":"MEMBER","auto_merge":null,"active_lock_reason":null,"merged":false,"mergeable":true,"rebaseable":true,"mergeable_state":"clean","merged_by":null,"comments":2,"review_comments":3,"maintainer_can_modify":false,"commits":4,"additions":10,"deletions":1,"changed_files":2}`
	const body2 = `{"url":"https://api.github.com/repos/o/r/pulls/5","id":123,"node_id":"PR_kwDO","html_url":"https://github.com/o/r/pull/5","diff_url":"https://github.com/o/r/pull/5.diff","patch_url":"https://github.com/o/r/pull/5.patch","issue_url":"https://api.github.com/repos/o/r/issues/5","number":5,"state":"open","locked":false,"title":"Full GitHub shape v2","user":{"login":"octocat","id":1,"node_id":"MDQ6VXNlcjE=","avatar_url":"https://avatars/u/1","html_url":"https://github.com/octocat","type":"User","site_admin":false},"body":"updated","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-03T00:00:00Z","closed_at":null,"merged_at":null,"merge_commit_sha":"abc","assignee":null,"assignees":[],"requested_reviewers":[],"requested_teams":[],"labels":[],"milestone":null,"draft":false,"commits_url":"https://api.github.com/repos/o/r/pulls/5/commits","review_comments_url":"https://api.github.com/repos/o/r/pulls/5/comments","review_comment_url":"https://api.github.com/repos/o/r/pulls/comments{/number}","comments_url":"https://api.github.com/repos/o/r/issues/5/comments","statuses_url":"https://api.github.com/repos/o/r/statuses/abc","head":{"label":"o:feature","ref":"feature","sha":"abc","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"base":{"label":"o:main","ref":"main","sha":"def","user":{"login":"o"},"repo":{"id":1,"name":"r","full_name":"o/r","private":false}},"_links":{"self":{"href":"https://api.github.com/repos/o/r/pulls/5"},"html":{"href":"https://github.com/o/r/pull/5"}},"author_association":"MEMBER","auto_merge":null,"active_lock_reason":null,"merged":false,"mergeable":true,"rebaseable":true,"mergeable_state":"clean","merged_by":null,"comments":2,"review_comments":3,"maintainer_can_modify":false,"commits":4,"additions":11,"deletions":1,"changed_files":2}`
	var prHits int32
	var gotAuth, gotPath string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = io.WriteString(w, `{"login":"testuser"}`)
			return
		}
		hit := atomic.AddInt32(&prHits, 1)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if hit == 1 {
			_, _ = io.WriteString(w, body1)
			return
		}
		_, _ = io.WriteString(w, body2)
	})
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), gh)

	for i := 0; i < 2; i++ {
		req := authedReq("GET", "/repos/o/r/pulls/5", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
		assertURLStrippedJSON(t, body1, w.Body.String())
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&prHits), "second request should be served from normalized cache")
	assert.Equal(t, "/repos/o/r/pulls/5", gotPath)
	assert.Equal(t, "Bearer "+testToken, gotAuth)

	webhookBody := []byte(`{"action":"synchronize","pull_request":{"number":5,"title":"Full GitHub shape v2","html_url":"https://github.com/o/r/pull/5","draft":false,"state":"open","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-03T00:00:00Z","head":{"ref":"feature","sha":"abc"},"base":{"ref":"main","repo":{"name":"r","owner":{"login":"o"}}},"labels":[]},"repository":{"name":"r","owner":{"login":"o"}}}`)
	hook := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(webhookBody))
	hook.Header.Set("X-GitHub-Event", "pull_request")
	hook.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, webhookBody))
	hookResp := httptest.NewRecorder()
	router.ServeHTTP(hookResp, hook)
	require.Equal(t, http.StatusAccepted, hookResp.Code)

	req := authedReq("GET", "/repos/o/r/pulls/5", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assertURLStrippedJSON(t, body2, w.Body.String())
	assert.Equal(t, int32(2), atomic.LoadInt32(&prHits))
}
