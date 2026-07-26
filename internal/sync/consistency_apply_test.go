package sync

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Apply-mode (reconcile) coverage: seeded drift -> CheckAndApply -> truth
// corrected, sticky against the next webhook; and the read-only guarantee of
// plain Check.

// TestConsistencyChecker_CheckIsReadOnly: with plenty of drift present, a
// plain Check writes NOTHING -- absent repos stay absent, stale PRs stay,
// poisoned statuses stay, visibility stays.
func TestConsistencyChecker_CheckIsReadOnly(t *testing.T) {
	srv := applyFake(t)
	checker, store, fresh := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	seedApplyDrift(t, store)

	_, err := checker.Check(ctx, "")
	require.NoError(t, err)

	_, err = store.GetRepo(ctx, "org1", "repo2")
	assert.ErrorIs(t, err, sql.ErrNoRows, "missing repo must NOT be absorbed by a read-only check")
	_, err = store.GetPullRequest(ctx, "org1", "repo1", 99)
	assert.NoError(t, err, "stale open PR must survive a read-only check")
	pr7, err := store.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.Equal(t, "PENDING", pr7.LastCommitStatus.String, "poisoned status must survive a read-only check")
	states, err := store.CommitCheckStates(ctx, "org1", "repo1", "abc123")
	require.NoError(t, err)
	assert.Len(t, states, 1, "ghost commit_checks row must survive a read-only check")
	leak, err := store.GetRepo(ctx, "org1", "repoLeak")
	require.NoError(t, err)
	assert.Equal(t, "public", leak.Visibility, "visibility must not change on a read-only check")
	meta, err := fresh.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "org1", Actor: AppInstallationActor(1)})
	require.NoError(t, err)
	assert.Nil(t, meta, "a read-only check stamps no freshness marker")
}

// applyFake is the live state the apply-mode tests run against:
//   - org1/repo1: default tip has NO rollup (cached says FAILURE -- the stuck
//     gcc/.github class); PR #7 (cached, ghost-PENDING poisoned, live SUCCESS,
//     auto-merge disarmed while the cache says squash) and PR #1 (not cached,
//     live SUCCESS, auto-merge MERGE armed).
//   - org1/repo2: on GitHub (private), not cached -> absorbed.
//   - org1/repoPub: cached private, live PUBLIC (drift toward open).
//   - org1/repoLeak: cached public, live PRIVATE (the leak direction).
func applyFake(t *testing.T) *httptest.Server {
	return consistencyFakeGitHub(t, map[string]fakeOwner{
		"org1": {
			repos: []map[string]any{
				liveRepo("org1", "repo1", "", []map[string]any{
					livePR(7, "abc123", "SUCCESS", ""),
					livePR(1, "sha1", "SUCCESS", "MERGE"),
				}),
				liveRepo("org1", "repo2", "SUCCESS", nil),
				liveRepo("org1", "repoPub", "SUCCESS", nil),
				liveRepo("org1", "repoLeak", "SUCCESS", nil),
			},
			vis: []map[string]any{
				visNode("repo1", "PUBLIC", false),
				visNode("repo2", "PRIVATE", false),
				visNode("repoPub", "PUBLIC", false),
				visNode("repoLeak", "PRIVATE", false),
			},
		},
		"someuser": {},
	})
}

// seedApplyDrift plants every drift class the apply must correct.
func seedApplyDrift(t *testing.T, store *ghdata.Store) {
	t.Helper()
	ctx := context.Background()
	old := time.Now().Add(-30 * time.Minute) // outside the reconcile grace window

	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		PushedAt:            sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "FAILURE", Valid: true}, // live tip has NO rollup
	}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repoPub", NameWithOwner: "org1/repoPub", Url: "https://github.com/org1/repoPub",
		Visibility:          "private", // live: PUBLIC
		PushedAt:            sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "SUCCESS", Valid: true},
	}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repoLeak", NameWithOwner: "org1/repoLeak", Url: "https://github.com/org1/repoLeak",
		Visibility:          "public", // live: PRIVATE -- the leak
		PushedAt:            sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "SUCCESS", Valid: true},
	}))

	// PR #7: REST-complete row (node_id set) with a stale armed auto-merge;
	// its head sha carries a ghost PENDING check row that pins the rollup.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7, Title: "Live title", Url: "https://github.com/org1/repo1/pull/1",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
		HeadRefOid:      sql.NullString{String: "abc123", Valid: true},
		NodeID:          sql.NullString{String: "node7", Valid: true},
		AutoMergeMethod: sql.NullString{String: "squash", Valid: true}, // live: disarmed
	}, old))
	_, err := store.ApplyCommitStatus(ctx, "org1", "repo1", "abc123", "check_run:test", "PENDING", false)
	require.NoError(t, err) // the ghost: its completion delivery was "missed"

	// PR #99: cached open, gone from GitHub, last touched long ago (outside
	// the reconcile grace) -> apply deletes it.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 99, Title: "Stale open", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
	}, old))
}

func TestConsistencyChecker_Apply(t *testing.T) {
	srv := applyFake(t)
	checker, store, fresh := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	seedApplyDrift(t, store)

	rep, err := checker.CheckAndApply(ctx, "org1")
	require.NoError(t, err)
	require.NotNil(t, rep.Applied)

	// The report's discrepancies describe the PRE-apply state.
	assert.NotNil(t, findDiscrepancy(rep, "org1/repo2", "", "only_on_github"))
	if d := findDiscrepancy(rep, "org1/repoLeak", "visibility", "visibility_leak"); assert.NotNil(t, d, "the leak direction gets its own issue") {
		assert.Equal(t, "public", d.Cached)
		assert.Equal(t, "private", d.GitHub)
	}
	assert.Equal(t, 1, rep.Summary.VisibilityLeaks)

	// Absorbed: repo2 exists now, visibility recorded from the checker map.
	repo2, err := store.GetRepo(ctx, "org1", "repo2")
	require.NoError(t, err, "missing repo must be absorbed")
	assert.Equal(t, "private", repo2.Visibility)

	// Absorbed: PR #1 with its armed auto-merge (the upsert cannot carry it
	// from a GraphQL-shaped row; the explicit set must).
	pr1, err := store.GetPullRequest(ctx, "org1", "repo1", 1)
	require.NoError(t, err, "missing PR must be absorbed")
	assert.Equal(t, "merge", pr1.AutoMergeMethod.String)
	assert.Equal(t, "SUCCESS", pr1.LastCommitStatus.String)

	// Deleted: the stale open PR.
	_, err = store.GetPullRequest(ctx, "org1", "repo1", 99)
	assert.ErrorIs(t, err, sql.ErrNoRows, "stale cached-open PR must be reconciled away")

	// Corrected: the poisoned rollup -- ghost rows deleted, GitHub's verdict set.
	pr7, err := store.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.Equal(t, "SUCCESS", pr7.LastCommitStatus.String, "GitHub's terminal rollup wins")
	assert.False(t, pr7.AutoMergeMethod.Valid, "stale armed auto-merge must be cleared")
	states, err := store.CommitCheckStates(ctx, "org1", "repo1", "abc123")
	require.NoError(t, err)
	assert.Empty(t, states, "contradicted commit_checks rows must be deleted, never synthesized")

	// Corrected: visibility both directions.
	pub, err := store.GetRepo(ctx, "org1", "repoPub")
	require.NoError(t, err)
	assert.Equal(t, "public", pub.Visibility)
	leak, err := store.GetRepo(ctx, "org1", "repoLeak")
	require.NoError(t, err)
	assert.Equal(t, "private", leak.Visibility, "the leak is closed")

	// Corrected: default_branch_status set to NULL (the COALESCE upsert can
	// never write this).
	repo1, err := store.GetRepo(ctx, "org1", "repo1")
	require.NoError(t, err)
	assert.False(t, repo1.DefaultBranchStatus.Valid, "a tip with no rollup must read NULL, not the stale FAILURE")
	assert.Equal(t, "public", repo1.Visibility, "fail-closed '' upgraded to GitHub's answer")

	// The tally.
	ap := rep.Applied
	assert.Equal(t, 1, ap.ReposAbsorbed, "repo2")
	assert.Equal(t, 1, ap.PRsAbsorbed, "PR #1")
	assert.Equal(t, 1, ap.PRsDeleted, "PR #99")
	assert.Equal(t, 4, ap.VisibilitySet, "repo1 ''->public, repo2 ''->private, repoPub, repoLeak")
	assert.Equal(t, 1, ap.StatusesCorrected, "PR #7 (PR #1 had no contradicted rows)")
	assert.Equal(t, 1, ap.CheckRowsDeleted, "the ghost PENDING row")
	assert.Equal(t, 1, ap.DefaultBranchStatusSet, "repo1 FAILURE->NULL")
	assert.Equal(t, 2, ap.AutoMergeSet, "PR #7 cleared + PR #1 armed")

	// The apply stamped the installation's freshness marker.
	meta, err := fresh.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "org1", Actor: AppInstallationActor(1)})
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, freshness.StateFresh, meta.State)

	// The grants landed under the installation principal (list_sync).
	ok, err := store.HasGrant(ctx, AppInstallationActor(1), "org1", "repo2", time.Now())
	require.NoError(t, err)
	assert.True(t, ok)

	// STICKINESS: the next PR webhook must not re-poison the corrected status.
	// A pull_request payload carries no CI state; with the ghost rows gone,
	// UpsertPRWithChecks derives an empty rollup and its guard skips the
	// overwrite, while COALESCE preserves the set value.
	require.NoError(t, store.UpsertPRWithChecks(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7, Title: "Live title", Url: "https://github.com/org1/repo1/pull/1",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-03",
		HeadRefOid: sql.NullString{String: "abc123", Valid: true},
		NodeID:     sql.NullString{String: "node7", Valid: true},
	}, nil, time.Now()))
	pr7Again, err := store.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.Equal(t, "SUCCESS", pr7Again.LastCommitStatus.String, "a later PR webhook must NOT re-poison the corrected status")

	// A second apply over now-consistent truth corrects nothing.
	rep2, err := checker.CheckAndApply(ctx, "org1")
	require.NoError(t, err)
	assert.Zero(t, rep2.Applied.StatusesCorrected)
	assert.Zero(t, rep2.Applied.VisibilitySet)
	assert.Zero(t, rep2.Applied.DefaultBranchStatusSet)
	assert.Zero(t, rep2.Applied.AutoMergeSet)
	assert.Zero(t, rep2.Applied.PRsDeleted)
}
