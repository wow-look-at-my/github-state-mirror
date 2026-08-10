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

// Shared fixtures for the cached-route tests: the fake GitHub every route
// test drives, the router stack over it, and the assertions the tier-2
// contract is checked with. The route tests themselves live one file per
// route (respcache_labels_test.go, respcache_branches_test.go, ...); this
// file holds only what more than one of them needs.

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
	contentsHits     int32
	commitHits       int32
	mintHits         int32
	probeHits        int32
	pullFilesHits    int32
	branchesHits     int32
	gitRefHits       int32
	runJobsHits      int32
	codeQualityHits  int32
	jobHits          int32
	labelHits        int32
	installReposHits int32
	hooksHits        int32
	// contents answers GET /repos/... contents paths; settable per test.
	contents func(w http.ResponseWriter, r *http.Request)
	// pullFiles answers GET /repos/{o}/{r}/pulls/{n}/files; settable per test.
	pullFiles func(w http.ResponseWriter, r *http.Request)
	// branches answers GET /repos/{o}/{r}/branches; settable per test.
	branches func(w http.ResponseWriter, r *http.Request)
	// gitRef answers GET /repos/{o}/{r}/git/ref/{ref}; settable per test
	// (the verdict tests answer 404).
	gitRef func(w http.ResponseWriter, r *http.Request)
	// runJobs answers GET /repos/{o}/{r}/actions/runs/{id}/jobs and job
	// answers GET /repos/{o}/{r}/actions/jobs/{id}; settable per test (the
	// live-job tests answer an in_progress job).
	runJobs func(w http.ResponseWriter, r *http.Request)
	job     func(w http.ResponseWriter, r *http.Request)
	// codeQuality answers GET/PATCH /repos/{o}/{r}/code-quality/setup;
	// settable per test (the relay tests answer 403).
	codeQuality func(w http.ResponseWriter, r *http.Request)
	// gitCommit answers GET /repos/{o}/{r}/git/commits/{sha}; settable per
	// test (the miss-marker tests answer 404).
	gitCommit func(w http.ResponseWriter, r *http.Request)
	// label answers GET/PATCH/DELETE /repos/{o}/{r}/labels/{name}; settable
	// per test (the write tests recolour it).
	label func(w http.ResponseWriter, r *http.Request)
	// installRepos answers GET /installation/repositories; settable per test
	// (the per-credential test varies the body by bearer).
	installRepos func(w http.ResponseWriter, r *http.Request)
	// hooks answers the repo and org hook listings AND their write verbs;
	// settable per test (the refusal test answers 403).
	hooks func(w http.ResponseWriter, r *http.Request)
	// probe answers the reveal probe (GET /repos/{owner}/{repo}); settable
	// per test. The default reports a PRIVATE repo, so callers earn grants.
	// The bare-repo route's miss fetches land here too, so probeHits counts
	// BOTH reveal probes AND cachedRepo fetches.
	probe func(w http.ResponseWriter, r *http.Request)
	// tokenExpiry is the expires_at minted tokens carry.
	tokenExpiry time.Time
}

func newRespCacheUpstream() *respCacheUpstream {
	u := &respCacheUpstream{tokenExpiry: time.Now().Add(time.Hour)}
	u.probe = func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		writeGitHubJSON(w, map[string]any{
			"name": parts[1], "full_name": parts[0] + "/" + parts[1],
			"private": true, "visibility": "private",
			"html_url":       "https://github.com/" + parts[0] + "/" + parts[1],
			"default_branch": "main",
			"owner": map[string]any{
				"login": parts[0], "avatar_url": "https://a",
				"html_url": "https://github.com/" + parts[0],
			},
		})
	}
	u.contents = func(w http.ResponseWriter, r *http.Request) {
		n := atomic.LoadInt32(&u.contentsHits)
		writeGitHubJSON(w, map[string]any{
			"type": "file", "encoding": "base64", "size": 5,
			"name": "cfg.jsonc", "path": ".github/cfg.jsonc",
			"content": "aGVsbG8=\n", "sha": fmt.Sprintf("%040d", n),
			"url": "https://api.github.com/x", "git_url": "https://api.github.com/y",
			"html_url": "https://github.com/z", "download_url": "https://raw.github.com/w",
			"_links": map[string]any{"self": "https://api.github.com/x"},
		})
	}
	u.gitCommit = func(w http.ResponseWriter, r *http.Request) {
		writeGitHubJSON(w, map[string]any{
			"sha": shaCommit, "node_id": "C_kwAE",
			"url":       "https://api.github.com/repos/org1/repo1/git/commits/x",
			"html_url":  "https://github.com/org1/repo1/commit/x",
			"author":    map[string]any{"name": "Alice", "email": "alice@example.com", "date": "2026-07-01T10:00:00Z"},
			"committer": map[string]any{"name": "Bob", "email": "bob@example.com", "date": "2026-07-01T10:05:00Z"},
			"tree":      map[string]any{"sha": shaTree1, "url": "https://api.github.com/trees/x"},
			"message":   "fix: a thing <with> & symbols",
			"parents": []any{map[string]any{
				"sha": shaBase, "url": "https://api.github.com/parent", "html_url": "https://github.com/parent",
			}},
			"verification": map[string]any{"verified": false, "reason": "unsigned"},
		})
	}
	// The URL-stuffed default bodies live next to their route tests:
	// respcache_pullfiles_test.go / respcache_branches_test.go.
	u.pullFiles = defaultPullFilesUpstream
	u.branches = defaultBranchesUpstream
	u.gitRef = defaultGitRefUpstream
	u.runJobs = defaultRunJobsUpstream
	u.job = defaultJobUpstream
	u.codeQuality = defaultCodeQualityUpstream
	u.label = defaultLabelUpstream
	u.installRepos = defaultInstallationReposUpstream
	u.hooks = defaultHooksUpstream
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
			// The reveal probe (and the cachedRepo route's miss fetch): is
			// this repo visible to the caller's token? Anchored, so the
			// deeper-path cases below can never be shadowed by it.
			atomic.AddInt32(&u.probeHits, 1)
			u.probe(w, r)
		case strings.Contains(r.URL.Path, "/pulls/") && strings.HasSuffix(r.URL.Path, "/files"):
			atomic.AddInt32(&u.pullFilesHits, 1)
			u.pullFiles(w, r)
		case strings.HasSuffix(r.URL.Path, "/branches"):
			atomic.AddInt32(&u.branchesHits, 1)
			u.branches(w, r)
		case strings.Contains(r.URL.Path, "/contents/"):
			atomic.AddInt32(&u.contentsHits, 1)
			u.contents(w, r)
		case strings.HasSuffix(r.URL.Path, "/code-quality/setup"):
			atomic.AddInt32(&u.codeQualityHits, 1)
			u.codeQuality(w, r)
		case strings.Contains(r.URL.Path, "/labels/"):
			atomic.AddInt32(&u.labelHits, 1)
			u.label(w, r)
		case r.URL.Path == "/installation/repositories":
			atomic.AddInt32(&u.installReposHits, 1)
			u.installRepos(w, r)
		case strings.Contains(r.URL.Path, "/hooks"):
			// Counts the READS only: a write is proxied, and counting it would
			// make "a hit did not call upstream" unreadable in the flush tests.
			if r.Method == http.MethodGet {
				atomic.AddInt32(&u.hooksHits, 1)
				u.hooks(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":12345678}`))
		case strings.Contains(r.URL.Path, "/actions/runs/") && strings.HasSuffix(r.URL.Path, "/jobs"):
			atomic.AddInt32(&u.runJobsHits, 1)
			u.runJobs(w, r)
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			atomic.AddInt32(&u.jobHits, 1)
			u.job(w, r)
		case strings.Contains(r.URL.Path, "/git/ref/"):
			atomic.AddInt32(&u.gitRefHits, 1)
			u.gitRef(w, r)
		case strings.Contains(r.URL.Path, "/git/commits/"):
			atomic.AddInt32(&u.commitHits, 1)
			u.gitCommit(w, r)
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			n := atomic.AddInt32(&u.mintHits, 1)
			w.WriteHeader(http.StatusCreated)
			writeGitHubJSON(w, map[string]any{
				"token":                fmt.Sprintf("ghs_minted%d", n),
				"expires_at":           u.tokenExpiry.UTC().Format(time.RFC3339),
				"permissions":          map[string]any{"contents": "read", "metadata": "read"},
				"repository_selection": "all",
			})
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

// writeJSON renders a fake-GitHub body. Every fixture builds its document as
// a Go value and marshals it: JSON text is never assembled from string pieces,
// because nothing about + or a format verb escapes what the value contains
// (internal/guards' marshalling check fails the build over it).
func writeGitHubJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// mustJSONString marshals a document a test needs as a string: an expected
// body, a request body, a raw delivery. It panics rather than returning an
// error because most callers are fake-upstream closures with no *testing.T in
// scope, and a fixture that cannot marshal is a broken test either way.
func mustJSONString(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(body)
}

// postWebhookJSON delivers a webhook whose payload is MARSHALLED from a Go
// value. Prefer it whenever a delivery carries a runtime value: splicing one
// between a JSON literal's own quotes escapes nothing (internal/guards'
// json-splice check fails the build over it), and a map literal reads as
// clearly as the document it becomes.
func postWebhookJSON(t *testing.T, router http.Handler, event string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	postWebhook(t, router, event, string(body))
}

// fixtureRepo is the `repository` object every fixture delivery carries.
func fixtureRepo() map[string]any {
	return map[string]any{
		"name":           "repo1",
		"owner":          map[string]any{"login": "org1"},
		"default_branch": "main",
	}
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
// the trimmed contract bans: url, *_url, or _links. `allowed` names EXACT key
// names exempted for that one route -- pinned exceptions for consumer-read
// link fields (the required-builds hook renders per-status `target_url` and
// per-run `details_url`/`html_url`, and reads the workflow-run `html_url`;
// consumer survey 2026-07-11). Adding an exception requires a fresh consumer
// survey, per CLAUDE.md; with no allowlist the ban is total, exactly as
// before.
func assertNoURLKeys(t *testing.T, body []byte, allowed ...string) {
	t.Helper()
	allow := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allow[strings.ToLower(k)] = true
	}
	var v interface{}
	require.NoError(t, json.Unmarshal(body, &v), "rebuilt body must be valid JSON: %s", body)
	var walk func(v interface{}, at string)
	walk = func(v interface{}, at string) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, val := range x {
				lk := strings.ToLower(k)
				assert.False(t, !allow[lk] && (lk == "url" || strings.HasSuffix(lk, "_url") || lk == "_links"),
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
