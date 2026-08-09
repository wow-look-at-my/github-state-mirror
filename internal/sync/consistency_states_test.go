package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Consistency-checker tests for the states a check can report other than
// drift: never-synced, transient-retry, served-now, filtering, rate limits,
// and the two unavailable paths.

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
