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

// These tests lock the merge-field stale guard: a base/head push un-resolves
// mergeable AND remembers the invalidated test-merge sha (merge_stale_sha),
// because a tip change always changes the sha of a SUCCESSFUL test merge --
// so an absorb path re-offering that exact sha within the window is presumed
// to be serving a pre-push answer (GitHub's recompute lag) and must not
// re-resolve the row, unless one of the two exemptions (the push-tip proof;
// the dirty-retained CONFLICTING pattern -- see the sections below) proves
// otherwise. The webhooks#66 incident: a lagged refetch re-resolved the
// invalidated sha, and every later read was a hit serving it frozen.

// The tests above the proof section pass after="" to NullPRMergeableByBranch:
// a marker WITHOUT the push-tip proof columns, which is exactly the old
// (reject-until-TTL) behavior -- and still a real production shape (a push
// payload with no usable after). The push-tip proof tests below cover the
// proof-recorded paths.
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

	// A list item with the recomputed sha stores it and clears the marker
	// (mergeable stays unresolved -- the list never carries it).
	item = restPR(7, "", staleShaB)
	require.NoError(t, s.AbsorbPullsList(ctx, "org1", "repo1", []dbgen.PullRequest{item}, nil, false, now, now.Add(2*time.Minute), time.Hour))
	row = getPR(t, s, 7)
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.Mergeable.Valid, "the list carries no mergeable; the row stays a single-route miss")
	assert.False(t, row.MergeStaleSha.Valid, "a fresh sha clears the marker")
}

// TestUpsertPRWithChecks_WebhookCannotResolveFromInvalidatedSha: an
// out-of-order pull_request delivery built before the push (resolved
// mergeable + the invalidated sha) must not re-resolve the row either -- the
// webhook upsert shares the SQL stale guard.
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

// TestSyncOrgTruth_CannotRearmMergeableOnShalessRow is the H3 lock: the
// GraphQL owner sync carries mergeable but never a test-merge sha, so it must
// not mark a REST-complete row resolved while the row's sha is null (a push
// just un-resolved it) -- that produced resolved-looking rows with a
// null/stale sha the single-PR route then served. Pure GraphQL rows (no
// node_id -- the /graphql tier's rows, never REST-servable) keep getting
// their mergeable from syncs.
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
	// sync's mergeable -- the /graphql tier serves it from truth.
	gql9 := gqlPR
	gql9.Number = 9
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(
		[]dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		map[string][]dbgen.PullRequest{"org1/repo1": {gqlPR, gql9}},
	), "user:1", now.Add(time.Minute), now.Add(time.Minute)))
	row9 := getPR(t, s, 9)
	assert.Equal(t, "MERGEABLE", row9.Mergeable.String, "a pure GraphQL row keeps taking the sync's mergeable")

	// Control 2: once REST re-resolves with a fresh sha, the sync may update
	// mergeable again (the old COALESCE behavior for sha-backed rows).
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

// TestNullPRMergeableByRepo: the unparseable-push fallback nulls merge fields
// on every open PR -- and deliberately records NO stale marker (the moved
// branch is unknown; an unmoved PR's re-offered sha is valid and must be
// re-absorbable immediately).
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

	// The same sha re-offered absorbs straight back in (the PR's branch may
	// never have moved).
	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, stale)
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, staleShaA, row.MergeCommitSha.String)
}

// ---- The push-tip proof (merge_stale_ref/merge_stale_after) ----

// TestNullPRMergeableByBranch_RecordsPushProof: the push-time un-resolve
// records WHICH branch moved and its after tip alongside the remembered sha,
// and a second push OVERWRITES the proof with its own after (an answer must
// reflect the NEWEST push to be provably post-push) while keeping the
// remembered sha (merge_commit_sha is already NULL).
func TestNullPRMergeableByBranch_RecordsPushProof(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now))
	row := getPR(t, s, 7)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String)
	assert.Equal(t, "main", row.MergeStaleRef.String, "the pushed branch must be recorded")
	assert.Equal(t, pushedBaseTip, row.MergeStaleAfter.String, "the push's after tip must be recorded")

	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip2, now.Add(time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "a second push must keep the remembered sha")
	assert.Equal(t, "main", row.MergeStaleRef.String)
	assert.Equal(t, pushedBaseTip2, row.MergeStaleAfter.String, "a second push must overwrite the proof with ITS after")
}

// TestNullPRMergeableByBranch_NoProofWithoutUsableAfter: an empty after (an
// unknowing caller) and git's all-zeros null id (a deleted ref) name no real
// tip, so the marker is recorded WITHOUT proof columns -- nothing can match
// them, and only the TTL unwedges (the old bound).
func TestNullPRMergeableByBranch_NoProofWithoutUsableAfter(t *testing.T) {
	for name, after := range map[string]string{"empty": "", "zeros": zeroSHA} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			now := time.Now()
			seedResolvedPR(t, s, now)

			require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", after, now))
			row := getPR(t, s, 7)
			assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the sha is still remembered")
			assert.True(t, row.MergeStaleAt.Valid, "the window is still stamped")
			assert.False(t, row.MergeStaleRef.Valid, "no usable after -> no proof branch")
			assert.False(t, row.MergeStaleAfter.Valid, "no usable after -> no proof tip")
		})
	}
}

// TestAbsorbSinglePull_WrongMarkHealsOnPushProof locks the wrong-mark race
// fix. The race: GitHub recomputes mergeability within seconds of a push once
// a read triggers it, and pr-minder polls the mirror right after pushing --
// so a poll-driven absorb can land GitHub's POST-push answer (fresh sha, base
// tip already at the push's after) BEFORE the push delivery reaches the
// mirror, and the late delivery then stamps that FRESH sha stale. Pre-fix
// this wedged the row for the whole MergeStaleTTL hour: every refetch
// re-offered the (correct!) sha, was rejected, and the route served
// mergeable:null while github.com showed it computed -- pr-minder's
// conflict-settle burned its full in-run ceiling on every touch of the PR.
// Now the re-offered answer's base tip equals the push's recorded after:
// post-push proof, accepted, marker fully cleared -- healed on the very next
// poll.
func TestAbsorbSinglePull_WrongMarkHealsOnPushProof(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// The poll-driven absorb lands GitHub's post-push answer first.
	postPush := restPR(7, "MERGEABLE", staleShaB)
	postPush.BaseRefOid = sql.NullString{String: pushedBaseTip, Valid: true}
	stale, err := s.AbsorbSinglePull(ctx, postPush, nil, now)
	require.NoError(t, err)
	require.False(t, stale)

	// The LATE push delivery arrives and wrongly marks the fresh sha stale.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now))
	row := getPR(t, s, 7)
	require.Equal(t, staleShaB, row.MergeStaleSha.String, "the wrong mark: the fresh sha is stamped stale")

	// The next poll re-offers the SAME (correct) answer: its base tip matches
	// the push's after, so it is provably post-push and must be accepted.
	stale, err = s.AbsorbSinglePull(ctx, postPush, nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, stale, "a post-push-proven answer must not be rejected")
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String, "the wrongly-marked sha must re-resolve on proof")
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid, "an accepted answer clears the whole marker")
	assert.False(t, row.MergeStaleAt.Valid)
	assert.False(t, row.MergeStaleRef.Valid)
	assert.False(t, row.MergeStaleAfter.Valid)
}

// TestAbsorbSinglePull_WrongMarkHealsOnHeadPushProof: the head-side variant --
// the wrong-marking push moved the PR's HEAD branch, so the proof matches on
// the answer's reported head tip instead.
func TestAbsorbSinglePull_WrongMarkHealsOnHeadPushProof(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	postPush := restPR(7, "MERGEABLE", staleShaB)
	postPush.HeadRefOid = sql.NullString{String: pushedHeadTip, Valid: true}
	stale, err := s.AbsorbSinglePull(ctx, postPush, nil, now)
	require.NoError(t, err)
	require.False(t, stale)

	// The late delivery of the HEAD push ("feature" is restPR's head branch).
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "feature", pushedHeadTip, now))
	row := getPR(t, s, 7)
	require.Equal(t, staleShaB, row.MergeStaleSha.String)
	require.Equal(t, "feature", row.MergeStaleRef.String)

	stale, err = s.AbsorbSinglePull(ctx, postPush, nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, stale, "the head-tip proof must accept the answer")
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid, "the marker clears in full")
	assert.False(t, row.MergeStaleAt.Valid)
	assert.False(t, row.MergeStaleRef.Valid)
	assert.False(t, row.MergeStaleAfter.Valid)
}

// TestAbsorbSinglePull_PrePushAnswerStillRejectedUnderProof: recording the
// proof must not weaken the guard's whole point -- a genuinely PRE-push
// answer (the invalidated sha with the OLD base tip) demonstrates nothing and
// is rejected exactly as before, marker and proof kept.
func TestAbsorbSinglePull_PrePushAnswerStillRejectedUnderProof(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	// The push moved the base to pushedBaseTip; the marker carries the proof.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now))

	// GitHub's recompute lags: the refetch re-offers the invalidated sha,
	// still reporting the OLD base tip (restPR's default) -- no proof.
	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, stale, "a pre-push answer must stay rejected")
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "a pre-push answer must not re-resolve mergeable")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the marker must survive the rejected absorb")
	assert.Equal(t, "main", row.MergeStaleRef.String, "and so must the proof")
	assert.Equal(t, pushedBaseTip, row.MergeStaleAfter.String)
}

// TestUpsertPRWithChecks_WebhookProofParity: the SQL stale guard (the
// webhook/list/sync writers' path) shares the tip proof with the Go check --
// a webhook-shaped upsert offering the marked sha resolves the row and clears
// the marker iff its reported tip matches the push's after.
func TestUpsertPRWithChecks_WebhookProofParity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// The wrong-mark state: the post-push answer absorbed, then stamped stale
	// by the late push delivery.
	postPush := restPR(7, "MERGEABLE", staleShaB)
	postPush.BaseRefOid = sql.NullString{String: pushedBaseTip, Valid: true}
	_, err := s.AbsorbSinglePull(ctx, postPush, nil, now)
	require.NoError(t, err)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now))

	// A payload with a NON-matching base tip: the SQL guard still rejects.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPR(7, "MERGEABLE", staleShaB), nil, now.Add(time.Minute)))
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "an unproven payload must stay rejected in SQL")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaB, row.MergeStaleSha.String)

	// The matching base tip: the SQL proof resolves the row + clears the
	// marker -- all four columns.
	require.NoError(t, s.UpsertPRWithChecks(ctx, postPush, nil, now.Add(2*time.Minute)))
	row = getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String, "the SQL tip proof must accept the payload")
	assert.Equal(t, staleShaB, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid)
	assert.False(t, row.MergeStaleAt.Valid)
	assert.False(t, row.MergeStaleRef.Valid)
	assert.False(t, row.MergeStaleAfter.Valid)
}

// ---- The dirty-retained CONFLICTING exemption ----
//
// The invariant behind the marker holds only for SUCCESSFUL test merges: a
// conflicted (dirty) PR gets NO new test merge, so GitHub keeps returning the
// RETAINED last-good merge_commit_sha with a fresh mergeable:false -- and its
// reported base.sha stays FROZEN at the last clean evaluation, so the
// push-tip proof cannot rescue it (live evidence 2026-07-17:
// wow-look-at-my/webhooks#44 and #124, both dirty on GitHub with retained
// shas and frozen base.sha values while the mirror served mergeable:null on
// consecutive miss-reads). Without the exemption, every base push over a
// conflicted PR deterministically wedged it to null for the whole
// MergeStaleTTL -- the reported pr-minder conflict-settle stall.

// TestAbsorbSinglePull_DirtyRetainedConflictHealsPastLagWindow: (the
// deterministic wedge fix) a CONFLICTING answer re-offering the marker sha is
// accepted once the marker outlives MergeStaleConflictingWindow -- replica
// lag can no longer explain the match, so the dirty-retained pattern is the
// only remaining explanation -- even when the push-tip proof is recorded but
// does NOT match the doc (the frozen-base.sha live case).
func TestAbsorbSinglePull_DirtyRetainedConflictHealsPastLagWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// The conflicted PR's stored state: CONFLICTING with the RETAINED sha.
	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "CONFLICTING", staleShaA), nil, now.Add(-2*time.Minute))
	require.NoError(t, err)
	require.False(t, stale)

	// A base push 60s ago: marker + proof recorded. The proof will NOT match
	// the dirty doc -- its base tip is frozen at the last clean evaluation.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now.Add(-time.Minute)))
	row := getPR(t, s, 7)
	require.Equal(t, staleShaA, row.MergeStaleSha.String)
	require.Equal(t, pushedBaseTip, row.MergeStaleAfter.String)

	// GitHub's post-push answer: still CONFLICTING, same retained sha, frozen
	// pre-push base tip (restPR's default != pushedBaseTip). Tip proof fails,
	// but the marker is past the lag window: accepted.
	stale, err = s.AbsorbSinglePull(ctx, restPR(7, "CONFLICTING", staleShaA), nil, now)
	require.NoError(t, err)
	assert.False(t, stale, "a dirty-retained CONFLICTING answer past the lag window must be accepted")
	row = getPR(t, s, 7)
	assert.Equal(t, "CONFLICTING", row.Mergeable.String, "the conflicted verdict must resolve the row")
	assert.Equal(t, staleShaA, row.MergeCommitSha.String, "the retained sha is stored")
	assert.False(t, row.MergeStaleSha.Valid, "the marker clears in full")
	assert.False(t, row.MergeStaleAt.Valid)
	assert.False(t, row.MergeStaleRef.Valid)
	assert.False(t, row.MergeStaleAfter.Valid)
}

// TestAbsorbSinglePull_ConflictSameShaInsideLagWindowRejected: within
// MergeStaleConflictingWindow a CONFLICTING same-sha answer could still be a
// genuinely pre-push read served by a lagging replica, so it stays rejected
// and the marker (proof included) survives.
func TestAbsorbSinglePull_ConflictSameShaInsideLagWindowRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "CONFLICTING", staleShaA), nil, now.Add(-2*time.Minute))
	require.NoError(t, err)
	require.False(t, stale)

	// The push landed 5s ago: still inside the lag window.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now.Add(-5*time.Second)))

	stale, err = s.AbsorbSinglePull(ctx, restPR(7, "CONFLICTING", staleShaA), nil, now)
	require.NoError(t, err)
	assert.True(t, stale, "inside the lag window the same-sha CONFLICTING answer must stay rejected")
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid)
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the marker must survive")
	assert.Equal(t, "main", row.MergeStaleRef.String)
	assert.Equal(t, pushedBaseTip, row.MergeStaleAfter.String)
}

// TestAbsorbSinglePull_MergeableSameShaKeepsFullTTL: the exemption is scoped
// to CONFLICTING only. A successful test merge really does always mint a new
// sha, so a MERGEABLE answer re-offering the marker sha is pre-push however
// old the marker is (within MergeStaleTTL) and stays rejected -- lag-window
// age buys it nothing.
func TestAbsorbSinglePull_MergeableSameShaKeepsFullTTL(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now.Add(-2*time.Minute))

	// The push landed 60s ago: well past the CONFLICTING lag window.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now.Add(-time.Minute)))

	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now)
	require.NoError(t, err)
	assert.True(t, stale, "a MERGEABLE same-sha answer must keep the full MergeStaleTTL rejection")
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid)
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the marker must survive")
}

// TestUpsertPRWithChecks_DirtyRetainedParity: the SQL stale guard shares the
// '-30 seconds' CONFLICTING exemption with the Go check -- through the
// webhook-shaped upsert path a MERGEABLE same-sha payload stays rejected past
// the lag window, while a CONFLICTING one resolves the row and clears the
// whole marker.
func TestUpsertPRWithChecks_DirtyRetainedParity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	stale, err := s.AbsorbSinglePull(ctx, restPR(7, "CONFLICTING", staleShaA), nil, now.Add(-2*time.Minute))
	require.NoError(t, err)
	require.False(t, stale)
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now.Add(-time.Minute)))

	// A MERGEABLE same-sha payload: still rejected by the SQL guard.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPR(7, "MERGEABLE", staleShaA), nil, now))
	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "SQL: a MERGEABLE same-sha payload must stay rejected")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleShaA, row.MergeStaleSha.String)

	// A CONFLICTING same-sha payload past the lag window: accepted by the SQL
	// exemption, marker cleared -- all four columns.
	require.NoError(t, s.UpsertPRWithChecks(ctx, restPR(7, "CONFLICTING", staleShaA), nil, now))
	row = getPR(t, s, 7)
	assert.Equal(t, "CONFLICTING", row.Mergeable.String, "SQL: the dirty-retained payload must be accepted")
	assert.Equal(t, staleShaA, row.MergeCommitSha.String)
	assert.False(t, row.MergeStaleSha.Valid)
	assert.False(t, row.MergeStaleAt.Valid)
	assert.False(t, row.MergeStaleRef.Valid)
	assert.False(t, row.MergeStaleAfter.Valid)
}

// TestPRMergeShaStale: the hit-gate helper (belt and braces -- the guarded
// writes null the sha rather than store it equal to the marker).
func TestPRMergeShaStale(t *testing.T) {
	now := time.Now()
	pr := restPR(7, "MERGEABLE", staleShaA)
	assert.False(t, PRMergeShaStale(pr, now), "no marker -> not stale")

	pr.MergeStaleSha = sql.NullString{String: staleShaA, Valid: true}
	pr.MergeStaleAt = sql.NullString{String: now.Add(-time.Minute).UTC().Format(time.RFC3339), Valid: true}
	assert.True(t, PRMergeShaStale(pr, now), "own sha == live marker -> stale")

	pr.MergeStaleAt = sql.NullString{String: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339), Valid: true}
	assert.False(t, PRMergeShaStale(pr, now), "expired marker -> not stale")

	pr.MergeStaleAt = sql.NullString{String: now.Add(-time.Minute).UTC().Format(time.RFC3339), Valid: true}
	pr.MergeCommitSha = sql.NullString{String: staleShaB, Valid: true}
	assert.False(t, PRMergeShaStale(pr, now), "a different sha -> not stale")
}
