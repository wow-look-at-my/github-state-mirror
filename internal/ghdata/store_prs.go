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

// UpsertPR merges one source's view of a PR into truth (see the query comment
// for the COALESCE semantics), stamping touched_at.
func (s *Store) UpsertPR(ctx context.Context, pr dbgen.PullRequest, now time.Time) error {
	return upsertPRTx(ctx, s.q, pr, rfc3339(now))
}

// UpsertPRWithChecks upserts a PR plus its labels and re-derives
// last_commit_status from the commit checks already recorded for the PR's head
// commit: a PR payload carries no CI state, so a PR first seen AFTER its head
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

	if err := upsertPRTx(ctx, q, pr, rfc3339(now)); err != nil {
		return err
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

// DeletePR removes a PR and its labels (a closed/merged PR leaves the cache).
func (s *Store) DeletePR(ctx context.Context, owner, repo string, number int64) error {
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
	return tx.Commit()
}

// zeroSHA is git's null object id -- what a push payload's after reads for a
// deleted ref. It never names a real tip, so it can never prove anything.
const zeroSHA = "0000000000000000000000000000000000000000"

// NullPRMergeableByBranch un-resolves mergeable (and the test-merge sha) on
// every open PR whose base or head is the pushed branch: GitHub recomputes
// mergeability when either side moves and never webhooks the result, so the
// last-known value is stale the moment the push lands. This keeps the
// single-PR route's known-mergeable gate honest (it misses and re-fetches
// instead of serving the pre-push answer). The nulled sha is remembered
// (merge_stale_sha, stamped at now): the pushed branch provably moved, so a
// refetch re-offering that exact sha is a pre-push answer and the absorb
// paths refuse to re-resolve from it (see MergeStaleTTL). after is the push's
// post-push tip sha: recorded with the branch (merge_stale_ref/
// merge_stale_after) it makes the marker VERIFIABLE -- an answer whose
// reported tip for the branch equals after provably post-dates the push and
// is accepted even when it re-offers the remembered sha, healing a WRONG mark
// (the marker stamped over an already-post-push sha by a late push delivery)
// on the very next poll. An empty or all-zeros after (a deleted ref, or an
// unknowing caller) records no proof: only the TTL unwedges then.
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

// NullPRMergeableOnTipMove un-resolves ONE PR's merge fields when the
// incoming webhook doc reports a moved tip against the stored row: the head
// sha changed (synchronize -- including fork heads, which emit no push
// webhook to run the per-branch un-resolve) or the base ref was retargeted.
// The payload's own moved-side ref+sha become the marker proof, exactly the
// push-path semantics, so the following upsert's stale guard treats the
// retained pre-move merge fields the payload carries as stale. Base ref OID
// drift alone never triggers: a conflicted PR's payload freezes base.sha at
// the last clean evaluation, so OID comparison would false-positive on every
// dirty PR. Returns whether it stamped.
func (s *Store) NullPRMergeableOnTipMove(ctx context.Context, incoming dbgen.PullRequest, now time.Time) (bool, error) {
	existing, err := s.q.GetPullRequest(ctx, dbgen.GetPullRequestParams{
		Owner: incoming.Owner, Repo: incoming.Repo, Number: incoming.Number,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // first sight: no stored merge fields to protect
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
		// No usable proof tip: stamp the marker without proof columns (the
		// TTL backstop still bounds it), never a half-filled proof.
		ref, after = sql.NullString{}, sql.NullString{}
	}
	return true, s.q.NullPRMergeableForPR(ctx, dbgen.NullPRMergeableForPRParams{
		StaleAt: rfc3339(now), StaleRef: ref, StaleAfter: after,
		Owner: incoming.Owner, Repo: incoming.Repo, Number: incoming.Number,
	})
}

// NullPRMergeableByRepo un-resolves merge fields on ALL the repo's open PRs --
// the conservative fallback for a push whose payload didn't parse (the ref is
// unknown, so any PR may be affected). It deliberately records NO
// merge_stale_sha: the stale-by-definition invariant holds only for a branch
// that provably moved, and marking an UNMOVED PR's sha stale would make its
// (still valid) re-offered sha unabsorbable for the whole window.
func (s *Store) NullPRMergeableByRepo(ctx context.Context, owner, repo string) error {
	return s.q.NullPRMergeableByRepo(ctx, dbgen.NullPRMergeableByRepoParams{
		Owner: owner, Repo: repo,
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
