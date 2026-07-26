package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// TestConsistencyChecker_ProgressEvents: the run reports its phase boundaries
// through the callback, in order -- start (with the owner count), then per
// owner an "owner" announcement followed by fetch/visibility/diffed (or a
// "skip" with the reason), and a final "done".
func TestConsistencyChecker_ProgressEvents(t *testing.T) {
	srv := driftFake(t)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()

	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "noinstall", Name: "x", NameWithOwner: "noinstall/x", Url: "u"}))
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "someuser", Name: "y", NameWithOwner: "someuser/y", Url: "u"}))

	var events []ProgressEvent
	rep, err := checker.CheckWithProgress(ctx, "", func(ev ProgressEvent) { events = append(events, ev) })
	require.NoError(t, err)
	require.NotNil(t, rep)

	// The run is framed by start (owner count known up front) and done.
	require.NotEmpty(t, events)
	assert.Equal(t, ProgressEvent{Phase: "start", Owners: 3}, events[0])
	assert.Equal(t, ProgressEvent{Phase: "done"}, events[len(events)-1])

	// Owners are announced in sorted order with 1-based positions.
	var announced []ProgressEvent
	for _, ev := range events {
		if ev.Phase == "owner" {
			announced = append(announced, ev)
		}
	}
	require.Len(t, announced, 3)
	assert.Equal(t, ProgressEvent{Phase: "owner", Owner: "noinstall", Index: 1, Total: 3}, announced[0])
	assert.Equal(t, ProgressEvent{Phase: "owner", Owner: "org1", Index: 2, Total: 3}, announced[1])
	assert.Equal(t, ProgressEvent{Phase: "owner", Owner: "someuser", Index: 3, Total: 3}, announced[2])

	// Per owner the phases arrive in pipeline order. phaseSeq collapses the
	// event list to one owner's phase sequence.
	phaseSeq := func(owner string) []string {
		var seq []string
		for _, ev := range events {
			if ev.Owner == owner {
				seq = append(seq, ev.Phase)
			}
		}
		return seq
	}
	assert.Equal(t, []string{"owner", "skip"}, phaseSeq("noinstall"))
	assert.Equal(t, []string{"owner", "fetch", "visibility", "diffed"}, phaseSeq("org1"))
	assert.Equal(t, []string{"owner", "fetch", "visibility", "diffed"}, phaseSeq("someuser"))

	for _, ev := range events {
		switch {
		case ev.Phase == "skip":
			assert.Equal(t, "noinstall", ev.Owner)
			assert.Contains(t, ev.Reason, "no GitHub App installation")
		case ev.Phase == "fetch" && ev.Owner == "org1":
			// driftFake serves org1's two repos in one page, totalCount included.
			assert.Equal(t, 2, ev.ReposFetched)
			assert.Equal(t, 2, ev.ReposTotal)
		case ev.Phase == "diffed" && ev.Owner == "someuser":
			// The LAST diffed event carries the running total across owners.
			assert.Equal(t, len(rep.Discrepancies), ev.Discrepancies)
		case ev.Phase == "applied":
			t.Errorf("a read-only check must not emit applied events")
		}
	}
}

// TestConsistencyChecker_ProgressNilSafe: a nil callback is explicitly
// supported (Check/CheckAndApply pass nil themselves).
func TestConsistencyChecker_ProgressNilSafe(t *testing.T) {
	srv := driftFake(t)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}))

	rep, err := checker.CheckWithProgress(ctx, "org1", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"org1"}, rep.OrgsChecked)
}

// TestConsistencyChecker_ProgressEventsApply: apply mode additionally emits a
// per-owner "applied" event carrying a SNAPSHOT of the corrections tally (not
// the live pointer later owners keep mutating).
func TestConsistencyChecker_ProgressEventsApply(t *testing.T) {
	srv := applyFake(t)
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()
	seedApplyDrift(t, store)

	var applied []ProgressEvent
	rep, err := checker.CheckAndApplyWithProgress(ctx, "org1", func(ev ProgressEvent) {
		if ev.Phase == "applied" {
			applied = append(applied, ev)
		}
	})
	require.NoError(t, err)
	require.NotNil(t, rep.Applied)

	require.Len(t, applied, 1)
	assert.Equal(t, "org1", applied[0].Owner)
	require.NotNil(t, applied[0].Applied)
	assert.Equal(t, *rep.Applied, *applied[0].Applied, "single owner: the snapshot equals the final tally")
	assert.NotSame(t, rep.Applied, applied[0].Applied, "the event must carry a copy, not the mutating report pointer")
}
