package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// fakeOwner is one owner's live state served by the fake GitHub.
type fakeOwner struct {
	repos []map[string]any // ownerDataQuery repo nodes
	vis   []map[string]any // ownerRepoVisibilityQuery nodes (includes archived)
}

// consistencyFakeGitHub serves the checker's fetch surface: installations
// (org1 = Organization, someuser = User), token mints, and a /graphql
// answering BOTH owner-agnostic queries (repositoryOwner data + visibility)
// per owner. The checker no longer sends organization() queries at all.
func consistencyFakeGitHub(t *testing.T, owners map[string]fakeOwner) *httptest.Server {
	return consistencyFakeGitHubFailing(t, owners, 0)
}

// consistencyFakeGitHubFailing is consistencyFakeGitHub with a transient-
// failure knob: the first fail502 /graphql requests answer 502 Bad Gateway
// before the normal responses resume (the client's bounded-retry path).
func consistencyFakeGitHubFailing(t *testing.T, owners map[string]fakeOwner, fail502 int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "account": map[string]any{"login": "org1", "type": "Organization"}},
			{"id": 2, "account": map[string]any{"login": "someuser", "type": "User"}},
		})
	})
	mux.HandleFunc("/app/installations/1/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_org1"})
	})
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_someuser"})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if fail502 > 0 {
			fail502--
			mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("Bad Gateway"))
			return
		}
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		require.Contains(t, req.Query, "repositoryOwner", "the checker must only issue owner-agnostic queries")
		owner, _ := req.Variables["owner"].(string)
		fo, ok := owners[owner]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repositoryOwner": nil}})
			return
		}
		nodes := fo.repos
		if !strings.Contains(req.Query, "pullRequests") {
			nodes = fo.vis // the visibility twin
		}
		if nodes == nil {
			nodes = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repositoryOwner": map[string]any{
					"repositories": map[string]any{
						"totalCount": len(nodes),
						"pageInfo":   map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes":      nodes,
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// liveRepo builds an ownerDataQuery repo node. status "" means no rollup on
// the default branch tip (statusCheckRollup null).
func liveRepo(owner, name, status string, prs []map[string]any) map[string]any {
	if prs == nil {
		prs = []map[string]any{}
	}
	var rollup any
	if status != "" {
		rollup = map[string]string{"state": status}
	}
	return map[string]any{
		"name":          name,
		"nameWithOwner": owner + "/" + name,
		"url":           "https://github.com/" + owner + "/" + name,
		"isDisabled":    false,
		"isArchived":    false,
		"pushedAt":      "2024-01-01T00:00:00Z",
		"owner":         map[string]string{"login": owner, "avatarUrl": "a", "url": "u"},
		"defaultBranchRef": map[string]any{
			"name":   "main",
			"target": map[string]any{"statusCheckRollup": rollup},
		},
		"pullRequests": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false},
			"nodes":    prs,
		},
	}
}

// livePR builds an ownerDataQuery PR node. rollup "" = no statusCheckRollup;
// autoMerge "" = not armed.
func livePR(number int, sha, rollup, autoMerge string) map[string]any {
	var rollupVal any
	if rollup != "" {
		rollupVal = map[string]string{"state": rollup}
	}
	pr := map[string]any{
		"number": number, "title": "Live title", "url": "https://github.com/org1/repo1/pull/1",
		"isDraft": false, "createdAt": "2024-01-01", "updatedAt": "2024-01-02",
		"additions": 10, "deletions": 5, "mergeable": "MERGEABLE",
		"headRefName": "feature", "baseRefName": "main", "headRefOid": sha,
		"author":         map[string]string{"login": "dev", "avatarUrl": "a", "url": "u"},
		"labels":         map[string]any{"nodes": []map[string]string{{"name": "bug", "color": "d73a4a"}}},
		"reviewRequests": map[string]int{"totalCount": 1},
		"commits": map[string]any{"nodes": []map[string]any{
			{"commit": map[string]any{"statusCheckRollup": rollupVal}},
		}},
	}
	if autoMerge != "" {
		pr["autoMergeRequest"] = map[string]string{"mergeMethod": autoMerge}
	}
	return pr
}

func visNode(name, visibility string, archived bool) map[string]any {
	return map[string]any{"name": name, "visibility": visibility, "isArchived": archived}
}

func newCheckerTest(t *testing.T, srvURL string) (*ConsistencyChecker, *ghdata.Store, *freshness.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	gh := ghclient.NewWithBaseURL(srvURL)
	// Zero backoff: a test exercising the transient-retry path must not sleep.
	gh.SetRetryBackoff([]time.Duration{0})
	store := ghdata.NewStore(db)
	fresh := freshness.NewStore(db)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)
	return NewConsistencyChecker(gh, store, fresh, app), store, fresh
}

// seedPullsListMarker plants a live "open-PR list complete" marker for a repo
// (the cached list route would serve from rows right now).
func seedPullsListMarker(t *testing.T, store *ghdata.Store, owner, repo string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, store.AbsorbPullsList(context.Background(), owner, repo, nil, nil, true, now, now, time.Hour))
}

// findDiscrepancy returns the first discrepancy matching repo+field (field may be
// "" to match only_in_cache / only_on_github entries by issue).
func findDiscrepancy(rep *ConsistencyReport, repo, field, issue string) *Discrepancy {
	for i := range rep.Discrepancies {
		d := &rep.Discrepancies[i]
		if d.Repo == repo && d.Field == field && d.Issue == issue {
			return d
		}
	}
	return nil
}

// driftFake is the live state the drift-detection tests run against.
func driftFake(t *testing.T) *httptest.Server {
	return consistencyFakeGitHub(t, map[string]fakeOwner{
		"org1": {
			repos: []map[string]any{
				liveRepo("org1", "repo1", "SUCCESS", []map[string]any{livePR(1, "abc123", "SUCCESS", "")}),
				liveRepo("org1", "repo2", "SUCCESS", nil),
			},
			vis: []map[string]any{
				visNode("repo1", "PUBLIC", false),
				visNode("repo2", "PRIVATE", false),
				visNode("old-archived", "PRIVATE", true),
			},
		},
		"someuser": {
			repos: []map[string]any{liveRepo("someuser", "dots", "SUCCESS", nil)},
			vis:   []map[string]any{visNode("dots", "PUBLIC", false)},
		},
	})
}

func TestConsistencyChecker_DetectsDrift(t *testing.T) {
	srv := driftFake(t)
	checker, store, fresh := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	now := time.Now()

	// A principal's org list-sync marker (the fetch that refreshes global
	// truth): error state after a failed refresh, with a known last successful
	// fetch. The report must surface it as the owner's truth freshness.
	lastFetched := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, fresh.Upsert(ctx, &freshness.Metadata{
		ResourceID:    freshness.ResourceID{Kind: KindOrgRepos, Key: "org1", Actor: "user:900"},
		State:         freshness.StateError,
		ErrorMessage:  "github api POST /graphql: 502 Bad Gateway",
		LastFetchedAt: &lastFetched,
	}))
	// A recorded identity for that principal: the report resolves the key to
	// its display name.
	require.NoError(t, store.RecordActorIdentity(ctx, "user:900", "octocat"))

	// Global truth that has drifted from the live state above.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		PushedAt:            sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "FAILURE", Valid: true}, // drift: live is SUCCESS
	}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "ghost", NameWithOwner: "org1/ghost", Url: "u", // not on GitHub at all
	}))
	// Archived on GitHub (per the visibility twin) but the cached row still
	// reads active: expected absence from org data + a missed-archive diff.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "old-archived", NameWithOwner: "org1/old-archived", Url: "u",
	}))
	// A cached-open PR under that archived repo: the orphan sweep must surface
	// it instead of letting it hide behind the repo-level entry.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "old-archived", Number: 5, Title: "Orphan", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
	}, now))
	// repo under an owner with no installation -> skipped, not a discrepancy.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "noinstall", Name: "x", NameWithOwner: "noinstall/x", Url: "u",
	}))
	// repo under the USER-account installation: since the owner-agnostic query
	// resolves users, someuser is CHECKED (no longer skipped) and y diffs as
	// only_in_cache.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "someuser", Name: "y", NameWithOwner: "someuser/y", Url: "u",
	}))

	// PR #1 cached with stale fields; PR #99 cached open but gone from GitHub.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 1, Title: "Old title", Url: "https://github.com/org1/repo1/pull/1",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02", IsDraft: 1,
		HeadRefOid:       sql.NullString{String: "abc123", Valid: true},
		LastCommitStatus: sql.NullString{String: "PENDING", Valid: true}, // drift: live is SUCCESS
	}, now))
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 99, Title: "Stale open", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
	}, now))
	require.NoError(t, store.SetPRLabels(ctx, "org1", "repo1", 1, []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "stale-label", Color: "ffffff"},
	}))

	rep, err := checker.Check(ctx, "")
	require.NoError(t, err)

	assert.Equal(t, "github-app", rep.FetchedAs)
	assert.Equal(t, []string{"org1", "someuser"}, rep.OrgsChecked, "user-account installations are checked, not skipped")
	assert.Nil(t, rep.Applied, "a read-only check reports no applied tally")

	// Only the ownerless repo is skipped.
	skipped := map[string]string{}
	for _, s := range rep.OrgsSkipped {
		skipped[s.Org] = s.Reason
	}
	assert.Contains(t, skipped, "noinstall")
	assert.NotContains(t, skipped, "someuser")

	// Repo-level drift.
	if d := findDiscrepancy(rep, "org1/ghost", "", "only_in_cache"); assert.NotNil(t, d, "ghost repo only in cache") {
		assert.False(t, d.Archived)
		assert.NotEmpty(t, d.Fix)
	}
	if d := findDiscrepancy(rep, "org1/old-archived", "", "only_in_cache"); assert.NotNil(t, d, "archived repo only in cache") {
		assert.True(t, d.Archived, "the visibility twin sees archived repos, so absence is classified")
		assert.Contains(t, d.Note, "archived")
	}
	if d := findDiscrepancy(rep, "org1/old-archived", "is_archived", "field_mismatch"); assert.NotNil(t, d, "missed archive webhook is its own diff") {
		assert.Equal(t, "false", d.Cached)
		assert.Equal(t, "true", d.GitHub)
	}
	assert.NotNil(t, findDiscrepancy(rep, "someuser/y", "", "only_in_cache"), "user-owned drift is diffed")
	if d := findDiscrepancy(rep, "org1/repo2", "", "only_on_github"); assert.NotNil(t, d, "repo2 only on github") {
		assert.Equal(t, "private", d.Visibility)
		assert.Contains(t, d.Note, "not yet absorbed")
	}
	assert.NotNil(t, findDiscrepancy(rep, "someuser/dots", "", "only_on_github"))
	if d := findDiscrepancy(rep, "org1/repo1", "default_branch_status", "field_mismatch"); assert.NotNil(t, d) {
		assert.Equal(t, "FAILURE", d.Cached)
		assert.Equal(t, "SUCCESS", d.GitHub)
	}
	// Cached visibility '' vs live public: informational, not a mismatch.
	if d := findDiscrepancy(rep, "org1/repo1", "visibility", "visibility_unknown"); assert.NotNil(t, d) {
		assert.Equal(t, "public", d.GitHub)
	}

	// Truth staleness metadata rides on the report header, attributed to the
	// principal whose sync marker it is.
	if sf, ok := rep.TruthFreshness["org1"]; assert.True(t, ok, "truth_freshness must include the checked org") {
		assert.Equal(t, "error", sf.State)
		assert.Equal(t, "2024-05-01T12:00:00Z", sf.LastFetchedAt)
		assert.Contains(t, sf.Error, "502 Bad Gateway")
		assert.Equal(t, "user:900", sf.Principal)
		assert.Equal(t, "octocat", sf.PrincipalName, "the recorded identity resolves the marker's principal")
	}

	// PR-level drift.
	if d := findDiscrepancy(rep, "org1/repo1", "last_commit_status", "field_mismatch"); assert.NotNil(t, d) {
		assert.Equal(t, "PENDING", d.Cached)
		assert.Equal(t, "SUCCESS", d.GitHub)
		assert.Contains(t, d.Fix, "commit_checks")
	}
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "title", "field_mismatch"), "title drift")
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "is_draft", "field_mismatch"), "draft drift")
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "label:stale-label", "field_mismatch"), "stale label only in cache")
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "label:bug", "field_mismatch"), "bug label only on github")

	// PR #99 (cached open, absent from GitHub's open set) carries the cached
	// row's detail for triage.
	if d := findDiscrepancy(rep, "org1/repo1", "", "only_in_cache"); assert.NotNil(t, d) {
		assert.Equal(t, int64(99), d.PR)
		assert.Equal(t, "Stale open", d.Title)
		assert.Equal(t, "2024-01-02", d.UpdatedAt)
		assert.NotEmpty(t, d.TouchedAt)
		assert.False(t, d.ServedNow, "no live pulls-list marker")
		assert.NotEmpty(t, d.Fix)
	}
	// The orphan PR under the archived (only_in_cache) repo is swept too.
	if d := findDiscrepancy(rep, "org1/old-archived", "", "only_in_cache"); assert.NotNil(t, d) {
		// (repo-level entry found above; find the PR entry specifically)
		found := false
		for _, dd := range rep.Discrepancies {
			if dd.Kind == "pr" && dd.Repo == "org1/old-archived" && dd.PR == 5 && dd.Issue == "only_in_cache" {
				found = true
				assert.Contains(t, dd.Note, "repo's own entry")
			}
		}
		assert.True(t, found, "cached-open PRs under only_in_cache repos must be swept")
	}

	// Summary tallies are internally consistent.
	assert.Equal(t, len(rep.Discrepancies), rep.Summary.Discrepancies)
	assert.Equal(t, 2, rep.Summary.OrgsChecked)
	assert.GreaterOrEqual(t, rep.Summary.ReposOnlyInCache, 2)
	assert.Equal(t, 1, rep.Summary.ReposOnlyInCacheArchived, "archived-absence is tallied separately")
	assert.GreaterOrEqual(t, rep.Summary.ReposOnlyOnGitHub, 2)
	assert.Equal(t, 1, rep.Summary.ReposOnlyOnGitHubPrivate, "the private missing repo is tallied separately")
	assert.GreaterOrEqual(t, rep.Summary.PRsOnlyInCache, 2)
	assert.GreaterOrEqual(t, rep.Summary.FieldMismatches, 3)
	assert.Zero(t, rep.Summary.VisibilityLeaks)
}

// TestConsistencyChecker_NeverSyncedFreshness: an owner with cached repos but
// ZERO freshness marker rows must appear in truth_freshness as never_synced --
// the silent omission is what hid "the fleet refresher never completed a
// cycle" in the 2026-07-13 report.
func TestConsistencyChecker_NeverSyncedFreshness(t *testing.T) {
	srv := driftFake(t)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u",
	}))

	rep, err := checker.Check(ctx, "org1")
	require.NoError(t, err)
	sf, ok := rep.TruthFreshness["org1"]
	require.True(t, ok, "a marker-less owner must still get a truth_freshness entry")
	assert.Equal(t, "never_synced", sf.State)
	assert.Empty(t, sf.LastFetchedAt)
	assert.Empty(t, sf.Principal)
	assert.Empty(t, sf.Error)
}

// TestConsistencyChecker_TransientFetchRetried: a single 502 on an owner's
// GraphQL fetch is retried by the client, so the owner is CHECKED -- not holed
// out of the report under orgs_skipped (the pre-retry behavior).
func TestConsistencyChecker_TransientFetchRetried(t *testing.T) {
	srv := consistencyFakeGitHubFailing(t, map[string]fakeOwner{
		"org1": {
			repos: []map[string]any{liveRepo("org1", "repo1", "SUCCESS", nil)},
			vis:   []map[string]any{visNode("repo1", "PUBLIC", false)},
		},
	}, 1)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
	}))

	rep, err := checker.Check(ctx, "org1")
	require.NoError(t, err)
	assert.Equal(t, []string{"org1"}, rep.OrgsChecked, "a once-502ing fetch must be retried, not skipped")
	assert.Empty(t, rep.OrgsSkipped)
}

// TestConsistencyChecker_ServedNow: a live pulls-list marker marks PR
// existence drift as actively served (the list route trusts the rows).
func TestConsistencyChecker_ServedNow(t *testing.T) {
	srv := driftFake(t)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()

	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		PushedAt: sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
	}))
	// PR #1 exists on GitHub but not in cache -- and the repo has a LIVE list
	// marker, so the (incomplete) cached list is served right now.
	seedPullsListMarker(t, store, "org1", "repo1")

	rep, err := checker.Check(ctx, "org1")
	require.NoError(t, err)
	found := false
	for _, d := range rep.Discrepancies {
		if d.Kind == "pr" && d.Repo == "org1/repo1" && d.PR == 1 && d.Issue == "only_on_github" {
			found = true
			assert.True(t, d.ServedNow, "a live marker means the wrong list is served now")
		}
	}
	assert.True(t, found)
}

func TestConsistencyChecker_OrgFilter(t *testing.T) {
	srv := driftFake(t)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}))
	// A second owner exists in truth but is excluded by the filter.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "someuser", Name: "y", NameWithOwner: "someuser/y", Url: "u"}))

	rep, err := checker.Check(ctx, "org1")
	require.NoError(t, err)
	assert.Equal(t, []string{"org1"}, rep.OrgsChecked)
	assert.Empty(t, rep.OrgsSkipped, "filtered-out owners are not even reported as skipped")
}

func TestConsistencyChecker_RateLimits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 7, "account": map[string]any{"login": "wow-look-at-my", "type": "Organization"}},
		})
	})
	mux.HandleFunc("/app/installations/7/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_7"})
	})
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resources": map[string]any{
				"graphql": map[string]any{"limit": 5000, "remaining": 4000, "used": 1000, "reset": 1999999999},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	checker, _, _ := newCheckerTest(t, srv.URL)
	limits, err := checker.RateLimits(context.Background())
	require.NoError(t, err)
	require.Len(t, limits, 1)
	assert.Equal(t, "wow-look-at-my", limits[0].Installation)
	assert.Equal(t, "Organization", limits[0].AccountType)
	assert.Empty(t, limits[0].Error)
	assert.Equal(t, 4000, limits[0].Resources["graphql"].Remaining)
	assert.Equal(t, int64(1999999999), limits[0].Resources["graphql"].Reset)
}

func TestConsistencyChecker_Unavailable(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	checker := NewConsistencyChecker(ghclient.New(), ghdata.NewStore(db), freshness.NewStore(db), nil)
	assert.False(t, checker.Available())
	_, err = checker.Check(context.Background(), "")
	assert.Error(t, err)
	_, err = checker.CheckAndApply(context.Background(), "")
	assert.Error(t, err)
}

func TestConsistencyChecker_InstallationsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	checker, store, _ := newCheckerTest(t, srv.URL)
	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}))

	_, err := checker.Check(context.Background(), "")
	assert.Error(t, err)
}
