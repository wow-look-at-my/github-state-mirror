package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rules in force on a branch, served by the embedded engine through the real router.

const branchRulesTarget = "/repos/org1/repo1/rules/branches/release/v1"

// branchRulesFixture is the router with the engine mounted, over a fake GitHub that counts rules calls.
type branchRulesFixture struct {
	router http.Handler
	up     *respCacheUpstream
	calls  atomic.Int32
	status atomic.Int32
	mu     sync.Mutex
	paths  []string
}

func newBranchRulesFixture(t *testing.T) *branchRulesFixture {
	t.Helper()
	f := &branchRulesFixture{up: newRespCacheUpstream()}
	f.status.Store(http.StatusOK)
	f.up.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.EscapedPath(), "/rules/branches/") {
			return false
		}
		f.calls.Add(1)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.EscapedPath())
		f.mu.Unlock()
		switch status := int(f.status.Load()); status {
		case http.StatusOK:
			writeGitHubJSON(w, []any{
				map[string]any{"type": "deletion", "ruleset_source_type": "Organization", "ruleset_source": "org1", "ruleset_id": 9},
				map[string]any{
					"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_source": "org1/repo1", "ruleset_id": 10,
					"parameters": map[string]any{"required_approving_review_count": 1},
					"url":        "https://api.github.com/repos/org1/repo1/rulesets/10",
					"html_url":   "https://github.com/org1/repo1/rules/10",
				},
			})
		case http.StatusUnauthorized:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("Bad credentials"))
		default:
			w.WriteHeader(status)
			writeGitHubJSON(w, map[string]any{"message": http.StatusText(status), "status": http.StatusText(status)})
		}
		return true
	}
	f.router = newFullTestStack(t, testAuth(), f.up.handler()).router
	return f
}

func (f *branchRulesFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.router, authedReq(http.MethodGet, target, nil))
}

func (f *branchRulesFixture) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[len(f.paths)-1]
}

func TestEngineBranchRules_MissThenHit(t *testing.T) {
	f := newBranchRulesFixture(t)

	w1 := f.get(t, branchRulesTarget+"?per_page=100&page=1")
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, mustJSONString([]any{
		map[string]any{"type": "deletion", "ruleset_source_type": "Organization", "ruleset_source": "org1", "ruleset_id": 9},
		map[string]any{"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_source": "org1/repo1", "ruleset_id": 10,
			"parameters": map[string]any{"required_approving_review_count": 1}},
	}), w1.Body.String())

	w2 := f.get(t, branchRulesTarget+"?page=1&per_page=100")
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.EqualValues(t, 1, f.calls.Load())

	// The default page is its own row.
	assert.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 2, f.calls.Load())
}

func TestEngineBranchRules_BranchWithSlashesIsOneKey(t *testing.T) {
	f := newBranchRulesFixture(t)
	require.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	assert.Equal(t, "/repos/org1/repo1/rules/branches/release/v1", f.lastPath(), "the whole branch reaches GitHub")
	assert.Equal(t, "hit", f.get(t, branchRulesTarget).Header().Get(cacheHeader))

	// Another branch under the same prefix, and the same name in another case, are their own rows.
	for _, other := range []string{"/repos/org1/repo1/rules/branches/release/v2", "/repos/org1/repo1/rules/branches/Release/v1"} {
		assert.Equal(t, "miss", f.get(t, other).Header().Get(cacheHeader), other)
	}
	assert.EqualValues(t, 3, f.calls.Load())

	// The owner and repo fold, so another spelling of them is the same row.
	assert.Equal(t, "hit", f.get(t, "/repos/Org1/Repo1/rules/branches/release/v1").Header().Get(cacheHeader))
	assert.EqualValues(t, 3, f.calls.Load())
}

func TestEngineBranchRules_EnterpriseSpellingReachesTheRoute(t *testing.T) {
	f := newBranchRulesFixture(t)
	assert.Equal(t, "miss", f.get(t, "/api/v3"+branchRulesTarget).Header().Get(cacheHeader))
	assert.Equal(t, "hit", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestEngineBranchRules_PrivateRepoNotRevealed(t *testing.T) {
	f := newBranchRulesFixture(t)
	f.up.probe = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeGitHubJSON(w, map[string]any{"message": "Not Found"})
	}
	for range 2 {
		w := f.get(t, branchRulesTarget)
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.NotContains(t, w.Body.String(), "ruleset_id")
	}
	assert.Zero(t, f.calls.Load(), "a refused caller never reaches the rules")
}

func TestEngineBranchRules_NotFoundIsStored(t *testing.T) {
	f := newBranchRulesFixture(t)
	f.status.Store(http.StatusNotFound)
	for i, want := range []string{"miss", "hit"} {
		w := f.get(t, branchRulesTarget)
		require.Equal(t, http.StatusNotFound, w.Code, "request %d", i)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestEngineBranchRules_ForbiddenIsTheCallersDenial(t *testing.T) {
	f := newBranchRulesFixture(t)
	f.status.Store(http.StatusForbidden)
	for i := range 2 {
		w := f.get(t, branchRulesTarget)
		require.Equal(t, http.StatusForbidden, w.Code, "request %d", i)
		assert.NotEqual(t, "hit", w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.EqualValues(t, 1, f.calls.Load(), "the second request is answered from the denial")
}

func TestEngineBranchRules_UnauthorizedIsNeverStored(t *testing.T) {
	f := newBranchRulesFixture(t)
	f.status.Store(http.StatusUnauthorized)
	for i := range 2 {
		w := f.get(t, branchRulesTarget)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "request %d", i)
		assert.NotEqual(t, "hit", w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.EqualValues(t, 2, f.calls.Load())
}

// A server failure is relayed once.
func TestEngineBranchRules_ServerFailureBacksOff(t *testing.T) {
	f := newBranchRulesFixture(t)
	f.status.Store(http.StatusInternalServerError)
	assert.Equal(t, http.StatusInternalServerError, f.get(t, branchRulesTarget).Code)
	assert.Equal(t, http.StatusBadGateway, f.get(t, branchRulesTarget).Code)
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestEngineBranchRules_UnmodeledShapesPassThrough(t *testing.T) {
	f := newBranchRulesFixture(t)
	targets := []string{
		branchRulesTarget + "?per_page=101", branchRulesTarget + "?page=11", branchRulesTarget + "?sort=name",
		branchRulesTarget + "?page=1&page=2",
		"/repos/org1/repo1/rules/branches/" + strings.Repeat("b", 256),
		"/repos/org1/repo1/rules/branches/bad%0Aname",
		"/repos/org1/repo1/rules/branches/bad%7Fname",
	}
	for _, target := range targets {
		w := f.get(t, target)
		h := w.Header().Get(cacheHeader)
		assert.True(t, h != "hit" && h != "miss", "%s answered %q", target, h)
	}
	req := authedReq(http.MethodGet, branchRulesTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Equal(t, "passthrough", do(t, f.router, req).Header().Get(cacheHeader))
	assert.EqualValues(t, len(targets)+1, f.calls.Load())

	// The longest name that fits, and an escaped byte that is not a control byte, are served.
	for _, target := range []string{
		"/repos/org1/repo1/rules/branches/" + strings.Repeat("b", 255),
		"/repos/org1/repo1/rules/branches/caf%C3%A9",
	} {
		assert.Equal(t, "miss", f.get(t, target).Header().Get(cacheHeader), target)
	}
}

// rulesetDelivery is a repository_ruleset delivery for a repository, or for an organization when repo is nil.
func rulesetDelivery(repo map[string]any, org string) map[string]any {
	p := map[string]any{
		"action":             "edited",
		"repository_ruleset": map[string]any{"id": 10, "updated_at": "2026-01-01T00:00:00Z"},
		"organization":       map[string]any{"login": org},
	}
	if repo != nil {
		p["repository"] = repo
	}
	return p
}

// A delivery that can change the merged rules drops the answer.
func TestEngineBranchRules_DeliveriesRefetch(t *testing.T) {
	f := newBranchRulesFixture(t)
	deliveries := []struct {
		event   string
		payload map[string]any
	}{
		{"repository_ruleset", rulesetDelivery(tagsRepo(1), "org1")},
		{"repository_ruleset", rulesetDelivery(nil, "org1")},
		{"repository_ruleset", rulesetDelivery(nil, "ORG1")},
		{"repository", map[string]any{"action": "renamed", "repository": tagsRepo(2)}},
	}
	for i, d := range deliveries {
		require.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader), "before delivery %d", i)
		require.Equal(t, "hit", f.get(t, branchRulesTarget).Header().Get(cacheHeader), "before delivery %d", i)
		postWebhookJSON(t, f.router, d.event, d.payload)
	}
	assert.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, len(deliveries)+1, f.calls.Load())
}

// A delete of an old ruleset after an edit of a newer one still drops the answer.
func TestEngineBranchRules_RulesetDeliveriesAreUnordered(t *testing.T) {
	f := newBranchRulesFixture(t)
	newer := rulesetDelivery(tagsRepo(1), "org1")
	newer["repository_ruleset"] = map[string]any{"id": 11, "updated_at": "2026-06-01T00:00:00Z"}
	older := rulesetDelivery(tagsRepo(1), "org1")
	older["action"] = "deleted"
	for i, d := range []map[string]any{newer, older} {
		require.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader), "before delivery %d", i)
		postWebhookJSON(t, f.router, "repository_ruleset", d)
	}
	assert.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 3, f.calls.Load())
}

// A delivery for another repository, another owner, or a push leaves the answer.
func TestEngineBranchRules_UnrelatedDeliveriesKeepTheAnswer(t *testing.T) {
	f := newBranchRulesFixture(t)
	require.Equal(t, "miss", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	other := tagsRepo(1)
	other["name"], other["full_name"] = "repo2", "org1/repo2"
	postWebhookJSON(t, f.router, "repository_ruleset", rulesetDelivery(other, "org1"))
	postWebhookJSON(t, f.router, "repository_ruleset", rulesetDelivery(nil, "org2"))
	postWebhookJSON(t, f.router, "push", map[string]any{
		"ref": "refs/heads/release/v1", "before": shaBase, "after": shaTip, "repository": tagsRepo(2), "commits": []any{},
	})
	assert.Equal(t, "hit", f.get(t, branchRulesTarget).Header().Get(cacheHeader))
	assert.EqualValues(t, 1, f.calls.Load())
}
