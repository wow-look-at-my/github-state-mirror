package ghdata

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two EXEMPTIONS that overrule the stale-sha presumption: the push-tip
// proof (an answer whose reported tip equals the push after sha post-dates
// the push) and the dirty-retained CONFLICTING pattern past replica lag.

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

	// Re-offers the same answer; its base tip matches the push's after, so it must be accepted.
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

	// GitHub's recompute lags: the refetch re-offers the invalidated sha with the OLD base tip.
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

	// The wrong-mark state: post-push answer absorbed, then stamped stale by the late delivery.
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

	// The matching base tip: the SQL proof resolves the row and clears the marker.
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

	// A base push 60s ago records marker + proof; the proof won't match the frozen dirty doc.
	require.NoError(t, s.NullPRMergeableByBranch(ctx, "org1", "repo1", "main", pushedBaseTip, now.Add(-time.Minute)))
	row := getPR(t, s, 7)
	require.Equal(t, staleShaA, row.MergeStaleSha.String)
	require.Equal(t, pushedBaseTip, row.MergeStaleAfter.String)

	// Still CONFLICTING with the frozen pre-push base tip: tip proof fails, but the lag window has passed.
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

	// A CONFLICTING same-sha payload past the lag window: accepted, marker cleared.
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
