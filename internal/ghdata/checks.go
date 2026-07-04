package ghdata

import (
	"context"
	"database/sql"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Commit-check aggregation (the webhook-fed CI rollup) plus the explicit-set
// correction levers the consistency check's apply mode uses. The COALESCE
// upserts deliberately cannot write the "cleared" states (a null rollup, a
// disarmed auto-merge); these direct sets are the only way to express them.

// ---- Commit checks ----

// ApplyCommitStatus records a single check/status state for a commit and
// recomputes the rollup, writing it onto any PR whose head is that commit and,
// when onDefaultBranch is set, onto the repo's default_branch_status. Returns
// the resulting rollup state.
func (s *Store) ApplyCommitStatus(ctx context.Context, owner, repo, sha, checkContext, state string, onDefaultBranch bool) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.UpsertCommitCheck(ctx, dbgen.UpsertCommitCheckParams{
		Owner: owner, Repo: repo, Sha: sha, Context: checkContext, State: state,
	}); err != nil {
		return "", err
	}
	states, err := q.ListCommitCheckStates(ctx, dbgen.ListCommitCheckStatesParams{
		Owner: owner, Repo: repo, Sha: sha,
	})
	if err != nil {
		return "", err
	}
	rollup := rollupState(states)
	status := sql.NullString{String: rollup, Valid: rollup != ""}
	if err := q.SetPRStatusByHeadSha(ctx, dbgen.SetPRStatusByHeadShaParams{
		LastCommitStatus: status,
		Owner:            owner,
		Repo:             repo,
		HeadRefOid:       sql.NullString{String: sha, Valid: sha != ""},
	}); err != nil {
		return "", err
	}
	if onDefaultBranch {
		if err := q.SetRepoDefaultBranchStatus(ctx, dbgen.SetRepoDefaultBranchStatusParams{
			DefaultBranchStatus: status,
			Owner:               owner,
			Name:                repo,
		}); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return rollup, nil
}

// DeleteCommitChecks drops the per-check rows for a commit (e.g. when the PR
// that pointed at it closes).
func (s *Store) DeleteCommitChecks(ctx context.Context, owner, repo, sha string) error {
	if sha == "" {
		return nil
	}
	return s.q.DeleteCommitChecksBySha(ctx, dbgen.DeleteCommitChecksByShaParams{Owner: owner, Repo: repo, Sha: sha})
}

// CommitCheckStates returns the recorded per-check states for a commit (the
// rollupState inputs). Read-only; the consistency check reads it to compare
// the webhook-aggregated verdict against GitHub's own rollup.
func (s *Store) CommitCheckStates(ctx context.Context, owner, repo, sha string) ([]string, error) {
	if sha == "" {
		return nil, nil
	}
	return s.q.ListCommitCheckStates(ctx, dbgen.ListCommitCheckStatesParams{Owner: owner, Repo: repo, Sha: sha})
}

// ForceCheckRollup replaces the webhook-aggregated check verdict for a commit
// with GitHub's DIRECTLY FETCHED statusCheckRollup: it deletes every
// commit_checks row for the sha (the contradicted aggregation -- typically a
// ghost PENDING row whose completion delivery was missed) and explicitly sets
// last_commit_status on the PRs heading that sha, INCLUDING NULL when GitHub
// reports no rollup at all. One transaction, so a racing check event lands
// strictly before or after the correction, never between the delete and the
// set.
//
// This is the correction that STICKS: with zero rows left for the sha, the
// next PR webhook's UpsertPRWithChecks derives an empty rollup and its
// rollup != "" guard skips the overwrite, while the PR-payload upsert itself
// carries no CI state (COALESCE preserves the set value). Deliberately no
// synthetic per-check rows are written -- a fabricated completed row would be
// tomorrow's ghost. Only a genuinely NEW check event (real state that should
// win) changes the verdict again.
func (s *Store) ForceCheckRollup(ctx context.Context, owner, repo, sha string, rollup sql.NullString) error {
	if sha == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeleteCommitChecksBySha(ctx, dbgen.DeleteCommitChecksByShaParams{Owner: owner, Repo: repo, Sha: sha}); err != nil {
		return err
	}
	if err := q.SetPRStatusByHeadSha(ctx, dbgen.SetPRStatusByHeadShaParams{
		LastCommitStatus: rollup,
		Owner:            owner,
		Repo:             repo,
		HeadRefOid:       sql.NullString{String: sha, Valid: true},
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetRepoDefaultBranchStatus overwrites a repo's default_branch_status with a
// directly fetched answer, INCLUDING NULL (no rollup on the current tip) --
// the SetPRMergeable idiom. The repo upsert COALESCEs this column, so a tip
// that advanced to a commit with no CI can never be cleared through it.
func (s *Store) SetRepoDefaultBranchStatus(ctx context.Context, owner, name string, status sql.NullString) error {
	return s.q.SetRepoDefaultBranchStatus(ctx, dbgen.SetRepoDefaultBranchStatusParams{
		DefaultBranchStatus: status,
		Owner:               owner,
		Name:                name,
	})
}

// SetPRAutoMergeMethod overwrites a PR's stored auto-merge method with a
// directly fetched answer, INCLUDING NULL (not armed). The upsert only takes
// this column from REST-shaped sources, so a stale armed flag (missed
// auto_merge_disabled delivery) is only ever cleared here.
func (s *Store) SetPRAutoMergeMethod(ctx context.Context, owner, repo string, number int64, method sql.NullString) error {
	return s.q.SetPRAutoMergeMethod(ctx, dbgen.SetPRAutoMergeMethodParams{
		AutoMergeMethod: method,
		Owner:           owner,
		Repo:            repo,
		Number:          number,
	})
}

// RollupState is the exported read of rollupState for the consistency
// check's apply mode, which compares this webhook-aggregated verdict against
// GitHub's directly fetched rollup (and corrects via ForceCheckRollup, never
// by changing this derivation).
func RollupState(states []string) string { return rollupState(states) }

// rollupState aggregates per-check states into a single GitHub-style rollup:
// any failure dominates, then pending, then success.
//
// A missed check-completion delivery leaves its commit_checks row stuck at
// PENDING, pinning the rollup at PENDING even after every real check finished.
// That is reconciled OUTSIDE this derivation: the consistency check's apply
// mode deletes contradicted rows and sets GitHub's own verdict
// (ForceCheckRollup) -- do not add reconciliation here.
func rollupState(states []string) string {
	var hasFailure, hasPending, hasSuccess bool
	for _, st := range states {
		switch st {
		case "FAILURE", "ERROR":
			hasFailure = true
		case "PENDING", "EXPECTED":
			hasPending = true
		case "SUCCESS":
			hasSuccess = true
		}
	}
	switch {
	case hasFailure:
		return "FAILURE"
	case hasPending:
		return "PENDING"
	case hasSuccess:
		return "SUCCESS"
	default:
		return ""
	}
}
