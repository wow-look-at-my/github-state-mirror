package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// ---- Pull Requests ----

func (s *Store) GetPullRequest(ctx context.Context, owner, repo string, number int64) (dbgen.PullRequest, error) {
	return s.q.GetPullRequest(ctx, dbgen.GetPullRequestParams{Owner: owner, Repo: repo, Number: number})
}

func (s *Store) ListOpenPRsByRepo(ctx context.Context, owner, repo string) ([]dbgen.PullRequest, error) {
	return s.q.ListOpenPullRequestsByRepo(ctx, dbgen.ListOpenPullRequestsByRepoParams{Owner: owner, Repo: repo})
}

// UpsertPR merges source's view of a PR into truth, stamping touched_at; a view that cannot prove it postdates a recorded close is refused.
func (s *Store) UpsertPR(ctx context.Context, pr dbgen.PullRequest, now time.Time) error {
	_, err := upsertPRTx(ctx, s.q, pr, rfc3339(now))
	return err
}

// UpsertPRWithChecks upserts a PR plus its labels and re-derives
// last_commit_status from the commit checks already recorded for the PR's head
// commit: a PR payload carries no CI state, so a PR seen AFTER its head
// commit's checks finished (e.g. a pr-minder auto-opened PR) would otherwise
// stay NULL until a later check event. When no checks are recorded the
// (COALESCE-preserved) status is left untouched.
func (s *Store) UpsertPRWithChecks(ctx context.Context, pr dbgen.PullRequest, labels []dbgen.PrLabel, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	applied, err := upsertPRTx(ctx, q, pr, rfc3339(now))
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	if pr.HeadRefOid.Valid && pr.HeadRefOid.String != "" {
		states, err := q.ListCommitCheckStates(ctx, dbgen.ListCommitCheckStatesParams{
			Owner: pr.Owner, Repo: pr.Repo, Sha: pr.HeadRefOid.String,
		})
		if err != nil {
			return err
		}
		if rollup := rollupState(states); rollup != "" {
			if err := q.SetPRStatusByHeadSha(ctx, dbgen.SetPRStatusByHeadShaParams{
				LastCommitStatus: sql.NullString{String: rollup, Valid: true},
				Owner:            pr.Owner,
				Repo:             pr.Repo,
				HeadRefOid:       pr.HeadRefOid,
			}); err != nil {
				return err
			}
		}
	}
	if err := replacePRLabelsTx(ctx, q, pr.Owner, pr.Repo, pr.Number, labels); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePR removes a PR and its labels (a closed/merged PR leaves the cache)
// and records the closure, at closedUpdatedAt -- the updated_at of the view
// that reported the close. Without that record the deleted row is simply
// absent, and absent loses to a later write carrying OLDER state: the PR
// comes back open and nothing restates the close.
func (s *Store) DeletePR(ctx context.Context, owner, repo string, number int64, closedUpdatedAt string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{Owner: owner, Repo: repo, PrNumber: number}); err != nil {
		return err
	}
	if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{Owner: owner, Repo: repo, Number: number}); err != nil {
		return err
	}
	if err := recordPRClosureTx(ctx, q, owner, repo, number, closedUpdatedAt, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.q.PrunePRClosures(ctx, rfc3339(now.Add(-PRClosureRetention)))
}

// zeroSHA is git's null object id for a deleted ref; it never names a real tip.
const zeroSHA = "0000000000000000000000000000000000000000"

// NullPRMergeableByBranch un-resolves mergeable (and remembers the nulled test-merge sha) on every open PR whose base or head is the pushed branch.
// see docs/cache/rest-routes.md
func (s *Store) NullPRMergeableByBranch(ctx context.Context, owner, repo, branch, after string, now time.Time) error {
	if branch == "" {
		return nil
	}
	var staleRef, staleAfter sql.NullString
	if after != "" && after != zeroSHA {
		staleRef = sql.NullString{String: branch, Valid: true}
		staleAfter = sql.NullString{String: after, Valid: true}
	}
	return s.q.NullPRMergeableByBranch(ctx, dbgen.NullPRMergeableByBranchParams{
		StaleAt:  rfc3339(now),
		StaleRef: staleRef, StaleAfter: staleAfter,
		Owner: owner, Repo: repo,
		BaseRefName: sql.NullString{String: branch, Valid: true},
		HeadRefName: sql.NullString{String: branch, Valid: true},
	})
}

// NullPRMergeableOnTipMove un-resolves PR's merge fields when the incoming webhook doc reports a moved tip against the stored row.
// see docs/cache/rest-routes.md
func (s *Store) NullPRMergeableOnTipMove(ctx context.Context, incoming dbgen.PullRequest, now time.Time) (bool, error) {
	existing, err := s.q.GetPullRequest(ctx, dbgen.GetPullRequestParams{
		Owner: incoming.Owner, Repo: incoming.Repo, Number: incoming.Number,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil //  sight: no stored merge fields to protect
	}
	if err != nil {
		return false, err
	}
	var ref, after sql.NullString
	switch {
	case incoming.HeadRefOid.Valid && incoming.HeadRefOid.String != "" &&
		existing.HeadRefOid.Valid && existing.HeadRefOid.String != "" &&
		incoming.HeadRefOid.String != existing.HeadRefOid.String:
		ref, after = incoming.HeadRefName, incoming.HeadRefOid
	case incoming.BaseRefName.Valid && incoming.BaseRefName.String != "" &&
		existing.BaseRefName.Valid && existing.BaseRefName.String != "" &&
		incoming.BaseRefName.String != existing.BaseRefName.String:
		ref, after = incoming.BaseRefName, incoming.BaseRefOid
	default:
		return false, nil
	}
	if !ref.Valid || ref.String == "" || !after.Valid || after.String == "" {
		// No usable proof tip: stamp without proof columns (TTL backstop still bounds it), never half-filled.
		ref, after = sql.NullString{}, sql.NullString{}
	}
	return true, s.q.NullPRMergeableForPR(ctx, dbgen.NullPRMergeableForPRParams{
		StaleAt: rfc3339(now), StaleRef: ref, StaleAfter: after,
		Owner: incoming.Owner, Repo: incoming.Repo, Number: incoming.Number,
	})
}

// NullPRMergeableByRepo un-resolves merge fields on ALL the repo's open PRs, deliberately marker-less -- the conservative fallback for an unparseable push.
// see docs/cache/rest-routes.md
func (s *Store) NullPRMergeableByRepo(ctx context.Context, owner, repo string) error {
	return s.q.NullPRMergeableByRepo(ctx, dbgen.NullPRMergeableByRepoParams{
		Owner: owner, Repo: repo,
	})
}

// NullPRMergeableStateByHeadSHA un-resolves mergeable_state alone for the open PRs on a head sha whose CI moved.
// see docs/cache/rest-routes.md
func (s *Store) NullPRMergeableStateByHeadSHA(ctx context.Context, owner, repo, headSHA string) error {
	if headSHA == "" {
		return nil
	}
	return s.q.NullPRMergeableStateByHeadSHA(ctx, dbgen.NullPRMergeableStateByHeadSHAParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		HeadRefOid: sql.NullString{String: headSHA, Valid: true},
	})
}

// ---- PR Labels ----

func (s *Store) SetPRLabels(ctx context.Context, owner, repo string, prNumber int64, labels []dbgen.PrLabel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := replacePRLabelsTx(ctx, q, owner, repo, prNumber, labels); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListPRLabels(ctx context.Context, owner, repo string, prNumber int64) ([]dbgen.PrLabel, error) {
	return s.q.ListPRLabels(ctx, dbgen.ListPRLabelsParams{Owner: owner, Repo: repo, PrNumber: prNumber})
}

// RecolorPRLabel updates the color of a label across all PRs in a repo.
func (s *Store) RecolorPRLabel(ctx context.Context, owner, repo, name, color string) error {
	return s.q.SetPRLabelColorByName(ctx, dbgen.SetPRLabelColorByNameParams{
		Color: color, Owner: owner, Repo: repo, Name: name,
	})
}

// DeletePRLabelByName removes a label from all PRs in a repo.
func (s *Store) DeletePRLabelByName(ctx context.Context, owner, repo, name string) error {
	return s.q.DeletePRLabelsByName(ctx, dbgen.DeletePRLabelsByNameParams{Owner: owner, Repo: repo, Name: name})
}

// SetRepoPushedAt updates a repo's pushed_at.
func (s *Store) SetRepoPushedAt(ctx context.Context, owner, repo, pushedAt string) error {
	return s.q.SetRepoPushedAt(ctx, dbgen.SetRepoPushedAtParams{
		PushedAt: sql.NullString{String: pushedAt, Valid: pushedAt != ""},
		Owner:    owner, Name: repo,
	})
}
