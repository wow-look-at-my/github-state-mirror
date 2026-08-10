package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// PRClosureRetention bounds how long a recorded close can refuse a write. It
// has to cover the window in which a stale view of the PR can still turn up:
// the delivery replayer's own lookback (internal/sync.ReplayLookback) is the
// outer bound on that, and nothing else re-sends a delivery at all.
const PRClosureRetention = 24 * time.Hour

// prClosureBlocks reports whether a recorded close refuses this view of the
// PR. Only open PRs are retained, so closing DELETES the row -- and an absent
// row cannot lose a comparison: a view carrying older state just re-inserts
// the PR as open, and nothing afterwards restates the close. The recorded
// close is what such a view has to beat.
//
// A view that provably postdates the close is a genuine reopen: it applies,
// and clears the record on the way through.
//
// A view with NO updated_at is refused. It cannot prove it postdates the
// close, and refusing what cannot prove it is the whole job of this record.
// Every real source stamps it (absorbRestPR rejects a PR object without one
// outright), so this is the fail-closed branch, not a live path.
func prClosureBlocks(ctx context.Context, q *dbgen.Queries, pr dbgen.PullRequest) (bool, error) {
	row, err := q.GetPRClosure(ctx, dbgen.GetPRClosureParams{
		Owner: NormalizeRepoKey(pr.Owner), Repo: NormalizeRepoKey(pr.Repo), Number: pr.Number,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pr.UpdatedAt == "" || pr.UpdatedAt <= row.UpdatedAt {
		slog.Warn("refused a PR write that does not postdate the recorded close",
			"owner", pr.Owner, "repo", pr.Repo, "number", pr.Number,
			"write_updated_at", pr.UpdatedAt, "closed_updated_at", row.UpdatedAt)
		return true, nil
	}
	return false, q.DeletePRClosure(ctx, dbgen.DeletePRClosureParams{
		Owner: NormalizeRepoKey(pr.Owner), Repo: NormalizeRepoKey(pr.Repo), Number: pr.Number,
	})
}

// recordPRClosureTx remembers that a PR left the cache closed, at the
// updated_at of the view that said so.
func recordPRClosureTx(ctx context.Context, q *dbgen.Queries, owner, repo string, number int64, closedUpdatedAt string, now time.Time) error {
	return q.RecordPRClosure(ctx, dbgen.RecordPRClosureParams{
		Owner:      NormalizeRepoKey(owner),
		Repo:       NormalizeRepoKey(repo),
		Number:     number,
		UpdatedAt:  closedUpdatedAt,
		RecordedAt: rfc3339(now),
	})
}
