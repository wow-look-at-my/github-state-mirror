package sync

import "context"

// Live progress for the consistency check / reconcile. A real fleet run takes
// minutes (per owner: a paginated repo+PR fetch at 5 repos/page, a visibility
// fetch, the diff, and in apply mode the corrections), so the run reports its
// phase boundaries through an optional, nil-safe callback. The streaming
// /api/cache/check?stream=1 handler relays these events to the operator as
// NDJSON lines; events are emitted synchronously, in order, on the run's own
// goroutine.

// ProgressEvent is one live-progress notification from a consistency run.
// Phase determines which of the optional fields are populated.
type ProgressEvent struct {
	// Phase: start | owner | fetch | visibility | diffed | applied | skip | done.
	Phase string `json:"phase"`
	// Owner is the owner being worked on (every phase except start/done).
	Owner string `json:"owner,omitempty"`
	// Index/Total: the owner's 1-based position in the run (phase=owner).
	Index int `json:"index,omitempty"`
	Total int `json:"total,omitempty"`
	// Owners is how many owners the run will visit (phase=start).
	Owners int `json:"owners,omitempty"`
	// ReposFetched is the cumulative repo count after a GetOwnerData page;
	// ReposTotal is the connection's totalCount, 0/absent when the server did
	// not report one (phase=fetch).
	ReposFetched int `json:"repos_fetched,omitempty"`
	ReposTotal   int `json:"repos_total,omitempty"`
	// Discrepancies is the running total across all owners diffed so far
	// (phase=diffed).
	Discrepancies int `json:"discrepancies,omitempty"`
	// Applied is a snapshot of the corrections tally so far (phase=applied,
	// apply mode only).
	Applied *AppliedSummary `json:"applied,omitempty"`
	// Reason says why the owner was skipped (phase=skip).
	Reason string `json:"reason,omitempty"`
}

// ProgressFunc receives progress events. A nil ProgressFunc is always safe --
// emit is the nil-checked send every emission site goes through.
type ProgressFunc func(ProgressEvent)

func (p ProgressFunc) emit(ev ProgressEvent) {
	if p != nil {
		p(ev)
	}
}

// CheckWithProgress is Check with a live progress callback (nil is allowed and
// behaves exactly like Check).
func (c *ConsistencyChecker) CheckWithProgress(ctx context.Context, orgFilter string, progress ProgressFunc) (*ConsistencyReport, error) {
	return c.run(ctx, orgFilter, false, progress)
}

// CheckAndApplyWithProgress is CheckAndApply with a live progress callback
// (nil is allowed and behaves exactly like CheckAndApply).
func (c *ConsistencyChecker) CheckAndApplyWithProgress(ctx context.Context, orgFilter string, progress ProgressFunc) (*ConsistencyReport, error) {
	return c.run(ctx, orgFilter, true, progress)
}
