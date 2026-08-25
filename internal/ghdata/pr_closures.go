package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

const PRClosureRetention = 24 * time.Hour

// prClosureBlocks reports whether a recorded close refuses this view of the PR; a view that cannot prove it postdates the close is refused.
// see docs/webhooks/delivery-gaps.md
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
