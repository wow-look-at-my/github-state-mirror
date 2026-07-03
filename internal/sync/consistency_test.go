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

// consistencyFakeGitHub returns a fake GitHub for the consistency check:
// two installations (org1 = Organization, someuser = User), and a /graphql that
// returns org1's live repos+PRs. someuser is never fetched (it's skipped).
func consistencyFakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
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
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		// The checker sends two queries: the shared org-data query and its own
		// checker-private repo-visibility query. Route on the latter's isPrivate
		// field. Live visibility: repo1 public, repo2 PRIVATE.
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "isPrivate") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"organization": map[string]any{
						"repositories": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
							"nodes": []map[string]any{
								{"name": "repo1", "isPrivate": false},
								{"name": "repo2", "isPrivate": true},
							},
						},
					},
				},
			})
			return
		}
		// org1 live state: repo1 (default branch status SUCCESS, PR #1 open with
		// label "bug" and a SUCCESS rollup) and repo2 (no PRs, not cached).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"organization": map[string]any{
					"repositories": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes": []map[string]any{
							liveRepo("repo1", "SUCCESS", []map[string]any{liveOpenPR(1)}),
							liveRepo("repo2", "SUCCESS", nil),
						},
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func liveRepo(name, status string, prs []map[string]any) map[string]any {
	if prs == nil {
		prs = []map[string]any{}
	}
	return map[string]any{
		"name":          name,
		"nameWithOwner": "org1/" + name,
		"url":           "https://github.com/org1/" + name,
		"isDisabled":    false,
		"pushedAt":      "2024-01-01T00:00:00Z",
		"owner":         map[string]string{"login": "org1", "avatarUrl": "a", "url": "u"},
		"defaultBranchRef": map[string]any{
			"name":   "main",
			"target": map[string]any{"statusCheckRollup": map[string]string{"state": status}},
		},
		"pullRequests": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false},
			"nodes":    prs,
		},
	}
}

func liveOpenPR(number int) map[string]any {
	return map[string]any{
		"number": number, "title": "Live title", "url": "https://github.com/org1/repo1/pull/1",
		"isDraft": false, "createdAt": "2024-01-01", "updatedAt": "2024-01-02",
		"additions": 10, "deletions": 5, "mergeable": "MERGEABLE",
		"headRefName": "feature", "baseRefName": "main", "headRefOid": "abc123",
		"author":         map[string]string{"login": "dev", "avatarUrl": "a", "url": "u"},
		"labels":         map[string]any{"nodes": []map[string]string{{"name": "bug", "color": "d73a4a"}}},
		"reviewRequests": map[string]int{"totalCount": 1},
		"commits": map[string]any{"nodes": []map[string]any{
			{"commit": map[string]any{"statusCheckRollup": map[string]string{"state": "SUCCESS"}}},
		}},
	}
}

func newCheckerTest(t *testing.T, srvURL string) (*ConsistencyChecker, *ghdata.Store, *freshness.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	gh := ghclient.NewWithBaseURL(srvURL)
	store := ghdata.NewStore(db)
	fresh := freshness.NewStore(db)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)
	return NewConsistencyChecker(gh, store, fresh, app), store, fresh
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

func TestConsistencyChecker_DetectsDrift(t *testing.T) {
	srv := consistencyFakeGitHub(t)
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

	// Global truth that has drifted from the live state above.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		DefaultBranchStatus: sql.NullString{String: "FAILURE", Valid: true}, // drift: live is SUCCESS
	}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "ghost", NameWithOwner: "org1/ghost", Url: "u", // not on GitHub
	}))
	// repo under an owner with no installation -> skipped, not a discrepancy.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "noinstall", Name: "x", NameWithOwner: "noinstall/x", Url: "u",
	}))
	// repo under a User-account installation -> skipped (org-repo fetch unsupported).
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "someuser", Name: "y", NameWithOwner: "someuser/y", Url: "u",
	}))

	// PR #1 cached with stale fields; PR #99 cached open but gone from GitHub.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 1, Title: "Old title", Url: "https://github.com/org1/repo1/pull/1",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02", IsDraft: 1,
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
	assert.Equal(t, []string{"org1"}, rep.OrgsChecked)

	// Both non-org owners were skipped with a reason (not reported as drift).
	skipped := map[string]string{}
	for _, s := range rep.OrgsSkipped {
		skipped[s.Org] = s.Reason
	}
	assert.Contains(t, skipped, "noinstall")
	assert.Contains(t, skipped, "someuser")

	// Repo-level drift.
	assert.NotNil(t, findDiscrepancy(rep, "org1/ghost", "", "only_in_cache"), "ghost repo only in cache")
	if d := findDiscrepancy(rep, "org1/repo2", "", "only_on_github"); assert.NotNil(t, d, "repo2 only on github") {
		// repo2 is PRIVATE on GitHub: under lazy global truth that means no
		// webhook and no principal's sync has absorbed it yet -- the report
		// must say so instead of implying a cache failure.
		assert.Equal(t, "private", d.Visibility)
		assert.Contains(t, d.Note, "not yet absorbed")
	}
	if d := findDiscrepancy(rep, "org1/repo1", "default_branch_status", "field_mismatch"); assert.NotNil(t, d) {
		assert.Equal(t, "FAILURE", d.Cached)
		assert.Equal(t, "SUCCESS", d.GitHub)
	}

	// Truth staleness metadata rides on the report header, attributed to the
	// principal whose sync marker it is.
	if sf, ok := rep.TruthFreshness["org1"]; assert.True(t, ok, "truth_freshness must include the checked org") {
		assert.Equal(t, "error", sf.State)
		assert.Equal(t, "2024-05-01T12:00:00Z", sf.LastFetchedAt)
		assert.Contains(t, sf.Error, "502 Bad Gateway")
		assert.Equal(t, "user:900", sf.Principal)
	}

	// PR-level drift.
	if d := findDiscrepancy(rep, "org1/repo1", "last_commit_status", "field_mismatch"); assert.NotNil(t, d) {
		assert.Equal(t, "PENDING", d.Cached)
		assert.Equal(t, "SUCCESS", d.GitHub)
	}
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "title", "field_mismatch"), "title drift")
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "is_draft", "field_mismatch"), "draft drift")
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "label:stale-label", "field_mismatch"), "stale label only in cache")
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo1", "label:bug", "field_mismatch"), "bug label only on github")

	// PR #99 (cached open, absent from GitHub's open set).
	if d := findDiscrepancy(rep, "org1/repo1", "", "only_in_cache"); assert.NotNil(t, d) {
		assert.Equal(t, int64(99), d.PR)
	}

	// Summary tallies are internally consistent.
	assert.Equal(t, len(rep.Discrepancies), rep.Summary.Discrepancies)
	assert.Equal(t, 1, rep.Summary.OrgsChecked)
	assert.GreaterOrEqual(t, rep.Summary.ReposOnlyInCache, 1)
	assert.GreaterOrEqual(t, rep.Summary.ReposOnlyOnGitHub, 1)
	assert.Equal(t, 1, rep.Summary.ReposOnlyOnGitHubPrivate, "the private missing repo is tallied separately")
	assert.GreaterOrEqual(t, rep.Summary.PRsOnlyInCache, 1)
	assert.GreaterOrEqual(t, rep.Summary.FieldMismatches, 3)
}

func TestConsistencyChecker_OrgFilter(t *testing.T) {
	srv := consistencyFakeGitHub(t)
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
