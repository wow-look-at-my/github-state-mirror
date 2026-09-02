package ghdata

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// The mergeable/mergeable_state pairing invariant: see docs/webhooks/invalidations.md.

// restPRState is restPR plus the mergeable_state facet.
func restPRState(number int64, mergeable, state, sha string) dbgen.PullRequest {
	pr := restPR(number, mergeable, sha)
	if state != "" {
		pr.MergeableState = sql.NullString{String: state, Valid: true}
	}
	return pr
}

// TestUpsertKeepsMergeAndStateTogether pins the duplicated stale-guard
// predicate in UpsertPullRequest: a SET clause reads the PRE-update row, so
// mergeable_state cannot be derived from what mergeable just became and each
// facet carries its own copy of the condition. Edit both branches or neither
// -- this test is what catches editing.
func TestUpsertKeepsMergeAndStateTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// Resolved: both facets present, sha-backed.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "MERGEABLE", "clean", staleShaA), nil, now))
	row := getPR(t, s, 7)
	require.Equal(t, "MERGEABLE", row.Mergeable.String)
	require.Equal(t, "clean", row.MergeableState.String)

	// A base push un-resolves the pair.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))
	row = getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "a base push must un-resolve mergeable")
	assert.False(t, row.MergeableState.Valid, "and mergeable_state with it -- 'clean' outlived the tip it described")

	// A lagged payload re-offering the invalidated sha resolves NEITHER.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "MERGEABLE", "clean", staleShaA), nil, now.Add(time.Minute)))
	row = getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "the stale guard must refuse mergeable")
	assert.False(t, row.MergeableState.Valid, "and must refuse mergeable_state on the same predicate")

	// GitHub recomputes: the pair resolves together, from payload.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "MERGEABLE", "behind", staleShaB), nil, now.Add(2*time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, "behind", row.MergeableState.String, "the recomputed state must land -- 'behind' is the field's whole purpose")

	// A payload carrying neither facet keeps what is stored (COALESCE).
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "", "", ""), nil, now.Add(3*time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, "behind", row.MergeableState.String, "an unopinionated payload must not blank the stored state")
}

// TestAbsorbSinglePullKeepsMergeAndStateTogether: the single-PR absorb writes
// the pair through SetPRMergeable, past the upsert, so it needs its own lock
// on the same invariant.
func TestAbsorbSinglePullKeepsMergeAndStateTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	stale, err := s.AbsorbSinglePull(ctx, restPRState(7, "MERGEABLE", "clean", staleShaA), nil, now)
	require.NoError(t, err)
	require.False(t, stale)
	require.Equal(t, "clean", getPR(t, s, 7).MergeableState.String)

	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))

	stale, err = s.AbsorbSinglePull(ctx, restPRState(7, "MERGEABLE", "clean", staleShaA), nil, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, stale)
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid)
	assert.False(t, row.MergeableState.Valid, "a rejected absorb must store the state unresolved too")

	stale, err = s.AbsorbSinglePull(ctx, restPRState(7, "MERGEABLE", "behind", staleShaB), nil, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, stale)
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, "behind", row.MergeableState.String)
}

// TestNullPRMergeableStateByHeadSHA: a check/status result is the ONLY thing
// that flips unstable/blocked <-> clean, and it moves no tip -- so this is the
//
//	merge facet with no other invalidator. It must null exactly that facet:
//
// nulling mergeable or the test-merge sha would make every check event
// re-fetch every PR on the sha for an answer that did not move.
func TestNullPRMergeableStateByHeadSHA(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	head := "1111111111111111111111111111111111111111" // restPR's head tip
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "MERGEABLE", "unstable", staleShaA), nil, now))

	// A PR on a different head sha, and a closed, are unaffected.
	other := restPRState(8, "MERGEABLE", "clean", staleShaB)
	other.HeadRefOid = sql.NullString{String: pushedHeadTip, Valid: true}
	require.NoError(t, s.UpsertPRWithChecks(ctx, other, nil, now))
	closed := restPRState(9, "MERGEABLE", "clean", staleShaB)
	closed.State = "CLOSED"
	require.NoError(t, s.UpsertPRWithChecks(ctx, closed, nil, now))

	require.NoError(t, s.NullPRMergeableStateByHeadSHA(ctx, "org1", "repo1", head))

	row := getPR(t, s, 7)
	assert.False(t, row.MergeableState.Valid, "the CI result un-resolves the state it may have moved")
	assert.Equal(t, "MERGEABLE", row.Mergeable.String, "a check result cannot change whether two trees conflict")
	assert.Equal(t, staleShaA, row.MergeCommitSha.String, "and it invalidates no test merge")
	assert.False(t, row.MergeStaleSha.Valid, "nothing pushed, so nothing is marked stale")

	assert.Equal(t, "clean", getPR(t, s, 8).MergeableState.String, "a PR on another head sha is untouched")
	assert.Equal(t, "clean", getPR(t, s, 9).MergeableState.String, "a closed PR is untouched")
}

// TestNullPRMergeableStateByHeadSHA_EmptySha: a delivery that named no sha
// must not become a repo-wide wildcard. The caller has already flushed what it
// could identify; blanking every open PR's state on top of that would cost a
// single-PR fetch per PR for an answer nothing said had changed.
func TestNullPRMergeableStateByHeadSHA_EmptySha(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "MERGEABLE", "clean", staleShaA), nil, now))

	require.NoError(t, s.NullPRMergeableStateByHeadSHA(ctx, "org1", "repo1", ""))
	assert.Equal(t, "clean", getPR(t, s, 7).MergeableState.String, "an empty sha is a no-op, never a wildcard")
}

// TestNullPRMergeableByRepo_NullsState: the unparseable-push fallback covers
// every open PR, and must take the state with it for the same reason the
// per-branch null does.
func TestNullPRMergeableByRepo_NullsState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPRState(7, "MERGEABLE", "clean", staleShaA), nil, now))

	require.NoError(t, s.NullPRMergeableByRepo(ctx, "org1", "repo1"))
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid)
	assert.False(t, row.MergeableState.Valid, "the repo-wide fallback must un-resolve the state too")
}
