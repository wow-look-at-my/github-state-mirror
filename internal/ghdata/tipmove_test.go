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

// These tests lock NullPRMergeableOnTipMove: the per-PR un-resolve for tip
// moves reported by the PR's OWN webhook payload. A fork head emits no push
// webhook, so without this the upsert's COALESCE preserved the retained
// pre-move mergeable next to the new head sha and the single-PR route served
// it frozen for the whole row TTL.

const (
	tipMoveNewHead = "9999999999999999999999999999999999999999" // the moved head tip
)

// synchronizePayload is the webhook-doc shape a synchronize delivery absorbs:
// node_id present, the NEW head sha, the RETAINED pre-move test-merge sha,
// and mergeable null (GitHub recomputing).
func synchronizePayload(number int64) dbgen.PullRequest {
	pr := restPR(number, "", staleShaA)
	pr.HeadRefOid = sql.NullString{String: tipMoveNewHead, Valid: true}
	return pr
}

// The head-move core: the stored row's resolved merge fields are nulled, the
// invalidated sha is remembered, and the payload's own head ref+sha land as
// the marker proof.
func TestNullPRMergeableOnTipMove_HeadMove(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	moved, err := s.NullPRMergeableOnTipMove(ctx, synchronizePayload(7), now)
	require.NoError(t, err)
	assert.True(t, moved, "a changed head sha must trigger the un-resolve")

	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid, "the retained mergeable must be un-resolved")
	assert.False(t, row.MergeCommitSha.Valid, "the test-merge sha must be nulled")
	assert.Equal(t, staleShaA, row.MergeStaleSha.String, "the invalidated sha must be remembered")
	assert.Equal(t, "feature", row.MergeStaleRef.String, "the moved head ref is the proof ref")
	assert.Equal(t, tipMoveNewHead, row.MergeStaleAfter.String, "the payload's new head sha is the proof tip")
}

// The freeze end-to-end: un-resolve then absorb the synchronize doc itself
// (the exact applyPRPayload sequence). The doc's tip proof accepts it, but
// its null mergeable cannot resurrect the retained answer -- the row ends
// UNRESOLVED with the new head, so the single-PR route misses and refetches
// instead of serving the pre-move answer.
func TestNullPRMergeableOnTipMove_SynchronizeAbsorbEndsUnresolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	doc := synchronizePayload(7)
	moved, err := s.NullPRMergeableOnTipMove(ctx, doc, now)
	require.NoError(t, err)
	require.True(t, moved)
	require.NoError(t, s.UpsertPRWithChecks(ctx, doc, nil, now))

	row := getPR(t, s, 7)
	assert.Equal(t, tipMoveNewHead, row.HeadRefOid.String, "the new head sha must land")
	assert.False(t, row.Mergeable.Valid, "the retained pre-move mergeable must NOT survive the absorb")
}

// Same-tip redeliveries are no-ops: a payload whose head sha matches the
// stored row must not disturb a resolved answer.
func TestNullPRMergeableOnTipMove_SameHeadNoOp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	moved, err := s.NullPRMergeableOnTipMove(ctx, restPR(7, "", staleShaA), now)
	require.NoError(t, err)
	assert.False(t, moved, "an unchanged head must not trigger")

	row := getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String, "the resolved answer must survive a redelivery")
	assert.Equal(t, staleShaA, row.MergeCommitSha.String)
}

// A base RETARGET (ref name change) invalidates the test merge like a head
// move; the proof is the new base ref and the payload's base tip.
func TestNullPRMergeableOnTipMove_BaseRetarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	doc := restPR(7, "", staleShaA)
	doc.BaseRefName = sql.NullString{String: "release-2.0", Valid: true}
	moved, err := s.NullPRMergeableOnTipMove(ctx, doc, now)
	require.NoError(t, err)
	assert.True(t, moved, "a retargeted base must trigger the un-resolve")

	row := getPR(t, s, 7)
	assert.False(t, row.Mergeable.Valid)
	assert.Equal(t, "release-2.0", row.MergeStaleRef.String)
}

// Base ref OID drift alone never triggers: a conflicted PR's payload freezes
// base.sha at the last clean evaluation, so OID comparison would
// false-positive on every dirty PR and wedge it unresolved.
func TestNullPRMergeableOnTipMove_BaseOidDriftIgnored(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	seedResolvedPR(t, s, now)

	doc := restPR(7, "", staleShaA)
	doc.BaseRefOid = sql.NullString{String: pushedBaseTip, Valid: true}
	moved, err := s.NullPRMergeableOnTipMove(ctx, doc, now)
	require.NoError(t, err)
	assert.False(t, moved, "base OID drift with an unchanged ref name must not trigger")

	row := getPR(t, s, 7)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String)
}

// First sight is a no-op: with no stored row there are no merge fields to
// protect (and nothing to compare against).
func TestNullPRMergeableOnTipMove_FirstSightNoOp(t *testing.T) {
	s := testStore(t)
	moved, err := s.NullPRMergeableOnTipMove(context.Background(), synchronizePayload(99), time.Now())
	require.NoError(t, err)
	assert.False(t, moved)
}
