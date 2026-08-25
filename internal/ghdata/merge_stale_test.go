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

// Locks the merge-field stale guard. see docs/cache/rest-routes.md
// (Merge-field invalidation semantics)
//
// Tests above the proof section pass after="" to NullPRMergeableByBranch: a
// marker without the push-tip proof columns, a real production shape (a push
// payload with no usable after). Tests below cover the proof-recorded paths.
const (
	staleShaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // the pre-push test-merge sha
	staleShaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // GitHub's recomputed sha

	// restPR's defaults are the PRE-push tips: base "2222...", head "1111...".
	pushedBaseTip  = "3333333333333333333333333333333333333333" // a base push's after tip
	pushedBaseTip2 = "5555555555555555555555555555555555555555" // a second base push's after
	pushedHeadTip  = "4444444444444444444444444444444444444444" // a head push's after tip
)

// restPR builds a REST/webhook-shaped row (node_id present) with a resolved
// mergeable and the given test-merge sha.
func restPR(number int64, mergeable, sha string) dbgen.PullRequest {
	pr := dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: number,
		Title: "t", Url: "u", State: "OPEN",
		CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
		NodeID:      sql.NullString{String: "PR_node", Valid: true},
		AuthorLogin: sql.NullString{String: "alice", Valid: true},
		HeadRefName: sql.NullString{String: "feature", Valid: true},
		BaseRefName: sql.NullString{String: "main", Valid: true},
		HeadRefOid:  sql.NullString{String: "1111111111111111111111111111111111111111", Valid: true},
		BaseRefOid:  sql.NullString{String: "2222222222222222222222222222222222222222", Valid: true},
	}
	if mergeable != "" {
		pr.Mergeable = sql.NullString{String: mergeable, Valid: true}
	}
	if sha != "" {
		pr.MergeCommitSha = sql.NullString{String: sha, Valid: true}
	}
	return pr
}

func getPR(t *testing.T, s *Store, number int64) dbgen.PullRequest {
	t.Helper()
	pr, err := s.GetPullRequest(context.Background(), "org1", "repo1", number)
	require.NoError(t, err)
	return pr
}

// seedResolvedPR absorbs a resolved single-PR answer, the state a push then
// invalidates.
func seedResolvedPR(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	stale, err := s.AbsorbSinglePull(context.Background(), restPR(7, "MERGEABLE", staleShaA), nil, now)
	require.NoError(t, err)
	require.False(t, stale)
	row := getPR(t, s, 7)
	require.Equal(t, "MERGEABLE", row.Mergeable.String)
	require.Equal(t, staleShaA, row.MergeCommitSha.String)
}

// TestNullPRMergeableByBranch_RemembersInvalidatedSha: the push-time
// un-resolve nulls mergeable + merge_commit_sha AND records the nulled sha
// with its stamp; a second push before re-resolution keeps the remembered sha
// (there is no newer one to remember).
func TestNullPRMergeableByBranch_RemembersInvalidatedSha(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "a base push must un-resolve mergeable")
	assert.False(t, row.MergeCommitSha.Valid, "a base push must null the test-merge sha")
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the invalidated sha must be remembered")
	assert.True(t, row.MergeStaleAt.Valid, "the marker must be stamped")

	// A second push while still unresolved: the remembered sha survives.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now.Add(time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "a second push must keep the remembered sha")
}

// TestAbsorbSinglePull_RejectsReofferedInvalidatedSha is the H2 core: after a
// push nulls the row, a refetch whose answer still carries the invalidated
// sha (GitHub's recompute lag) is stored UNRESOLVED -- the row keeps missing,
// each miss re-triggering the recompute -- until GitHub serves a NEW sha,
// which resolves the row and clears the marker.
func TestAbsorbSinglePull_RejectsReofferedInvalidatedSha(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))

	// GitHub re-offers the pre-push answer: rejected, stored unresolved.
	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, stale, "re-offering the invalidated sha must be reported stale")
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "a stale answer must not re-resolve mergeable")
	assert.False(t, row.MergeCommitSha.Valid, "the stale sha must not be stored")
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the marker must survive a rejected absorb")

	// GitHub recomputes: a NEW sha resolves the row and retires the marker.
	stale, err = s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaB), nil, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.False(t, stale)
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String, "a fresh sha must resolve the row")
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid, "a fresh sha must clear the marker")
	assert.False(t, row.MergeStaleAt.Valid)
}

// TestAbsorbSinglePull_NullShaAnswerKeepsMarker: a resolved CONFLICTING
// answer legitimately carries NO test-merge sha (a failed test merge). It is
// not same-sha-stale, so it resolves the row -- but the marker stays (no new
// sha vouched for a recompute), still guarding against a later lagged answer
// re-offering the invalidated sha.
func TestAbsorbSinglePull_NullShaAnswerKeepsMarker(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))

	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "CONFLICTING", ""), nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, stale, "a sha-less answer is not same-sha-stale")
	row := getPR(t, s, 7)
	assert.Equal(t, "CONFLICTING", row.Mergeable.String, "GitHub's own sha-less CONFLICTING answer is authoritative")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "no new sha appeared, so the marker stays")
}

// TestAbsorbSinglePull_ExpiredMarkerAccepts: past the marker window the
// re-offered sha is accepted again (and the marker cleared). The window is
// what stops a sha WRONGLY marked stale -- a fetch absorbed the fresh
// post-push sha before the late push delivery landed, which then stamped it
// -- from wedging the row into missing forever.
func TestAbsorbSinglePull_ExpiredMarkerAccepts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now.Add(-2*time.Hour))
	// The push (and its stamp) happened two hours ago; the window is 1h.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now.Add(-2*time.Hour)))

	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now)
	require.NoError(t, err)
	assert.False(t, stale, "an expired marker must not reject")
	row := getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, staleShaA, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid, "an accepted sha clears the expired marker")
}

// TestAbsorbPullsList_CannotStoreInvalidatedSha: the pulls-LIST absorb path
// carries merge_commit_sha (though never mergeable) -- re-offering the
// invalidated sha must not store it, or the list rebuild would serve a
// provably-stale sha and the sync re-arm (H3) would have a stale sha to pair
// with.
func TestAbsorbPullsList_CannotStoreInvalidatedSha(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))

	item := restPR(7, "", staleShaA) // list items carry no mergeable
	require.NoError(t, s.AbsorbPullsList(ctx, "org1", "repo1", []dbgen.PullRequest{item}, nil, false, now, now.Add(time.Minute), time.Hour))
	row := getPR(t, s, 7)
	assert.False(t, row.MergeCommitSha.Valid, "the list absorb must not store the invalidated sha")
	assert.False(t, row.Mergeable.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the marker must survive the list absorb")

	// The recomputed sha stores and clears the marker; the list carries no mergeable.
	item = restPR(7, "", staleShaB)
	require.NoError(t, s.AbsorbPullsList(ctx, "org1", "repo1", []dbgen.PullRequest{item}, nil, false, now, now.Add(2*time.Minute), time.Hour))
	row = getPR(t, s, 7)
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.Mergeable.Valid, "the list carries no mergeable; the row stays a single-route miss")
	assert.False(t, row.MergeStaleSha.Valid, "a fresh sha clears the marker")
}

// The webhook upsert shares the same SQL stale guard.
func TestUpsertPRWithChecks_WebhookCannotResolveFromInvalidatedSha(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))

	require.NoError(t, s.UpsertPRWithChecks(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now.Add(time.Minute)))
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "a stale webhook payload must not re-resolve mergeable")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String)

	// A payload carrying the recomputed sha resolves and clears the marker.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPR(7, "MERGEABLE", staleShaB), nil, now.Add(2*time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid)
}

// The sync must not resolve a REST-complete row whose sha is null (a push
// un-resolved it); pure GraphQL rows (no node_id) keep taking sync mergeable.
func TestSyncOrgTruth_CannotRearmMergeableOnShalessRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", "", now))

	// The periodic sync's snapshot was fetched pre-push: GraphQL-shaped
	// (node_id null), mergeable resolved.
	gqlPR := dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7, Title: "t", Url: "u", State: "OPEN",
		CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
		Mergeable:   sql.NullString{String: "MERGEABLE", Valid: true},
		AuthorLogin: sql.NullString{String: "alice", Valid: true},
	}
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(
		[]dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		map[string][]dbgen.PullRequest{"org1/repo1": {gqlPR}},
	), "user:1", now.Add(time.Minute), now.Add(time.Minute)))

	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "the sha-less sync must not re-arm mergeable on a push-nulled row")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, "PR_node", row.NodeID.String, "the sync must not degrade the row's REST columns")

	// Control 1: a PURE GraphQL row (never REST-fetched) still takes the
	gql9 := gqlPR
	gql9.Number = 9
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(
		[]dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		map[string][]dbgen.PullRequest{"org1/repo1": {gqlPR, gql9}},
	), "user:1", now.Add(time.Minute), now.Add(time.Minute)))
	row9 := getPR(t, s, 9)
	assert.Equal(t, "MERGEABLE", row9.Mergeable.String, "a pure GraphQL row keeps taking the sync's mergeable")

	// Control 2: once REST re-resolves with a fresh sha, the sync may update
	_, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaB), nil, now.Add(2*time.Minute))
	require.NoError(t, err)
	gqlConflicting := gqlPR
	gqlConflicting.Mergeable = sql.NullString{String: "CONFLICTING", Valid: true}
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(
		[]dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		map[string][]dbgen.PullRequest{"org1/repo1": {gqlConflicting}},
	), "user:1", now.Add(3*time.Minute), now.Add(3*time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, "CONFLICTING", row.Mergeable.String, "a sha-backed row still takes sync updates")
}

// The repo-wide fallback records NO stale marker: the moved branch is unknown.
func TestNullPRMergeableByRepo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	require.NoError(t, s.NullPRMergeableByRepo(ctx, "org1", "repo1"))
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "the repo-wide fallback must un-resolve mergeable")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.False(t, row.MergeStaleSha.Valid, "the repo-wide fallback must not mark a sha stale")

	// Re-offering the same sha absorbs straight back in.
	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, stale)
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, staleShaA, row.MergeCommitSha.String)
}

// ---- The push-tip proof (merge_stale_ref/merge_stale_after) ----

// A second push overwrites the proof with its own after tip, keeping the sha.
