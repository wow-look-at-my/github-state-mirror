package ghdata

import (
	"context"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Admin cache browse + consistency-check reads.
//
// These take the actor (cache partition) as an explicit argument rather than
// reading it from the request context, because the dashboard admin views and the
// consistency checker inspect a scope chosen by the operator, not the caller's
// own. They are read-only and gated to admins in the API layer. The data tables
// stay keyed by the opaque fingerprint — this does not relax isolation, it just
// lets the operator read what is already cached.

// ReposByActor returns every cached repo for one actor.
func (s *Store) ReposByActor(ctx context.Context, actorFP string) ([]dbgen.Repo, error) {
	return s.q.ListReposByActor(ctx, actorFP)
}

// PullRequestsByActor returns every cached PR (any state) for one actor.
func (s *Store) PullRequestsByActor(ctx context.Context, actorFP string) ([]dbgen.PullRequest, error) {
	return s.q.ListPullRequestsByActor(ctx, actorFP)
}

// OpenPullRequestsByActor returns only OPEN cached PRs for one actor (what the
// consistency check compares against GitHub's live open-PR set).
func (s *Store) OpenPullRequestsByActor(ctx context.Context, actorFP string) ([]dbgen.PullRequest, error) {
	return s.q.ListOpenPullRequestsByActor(ctx, actorFP)
}

// PRLabelsByActor returns every cached PR label for one actor.
func (s *Store) PRLabelsByActor(ctx context.Context, actorFP string) ([]dbgen.PrLabel, error) {
	return s.q.ListPRLabelsByActor(ctx, actorFP)
}

// UsersByActor returns every cached user for one actor.
func (s *Store) UsersByActor(ctx context.Context, actorFP string) ([]dbgen.User, error) {
	return s.q.ListUsersByActor(ctx, actorFP)
}

// OrgsByActor returns every cached org for one actor.
func (s *Store) OrgsByActor(ctx context.Context, actorFP string) ([]dbgen.Org, error) {
	return s.q.ListOrgsByActor(ctx, actorFP)
}

// PRFilesByActor returns every cached PR file row for one actor.
func (s *Store) PRFilesByActor(ctx context.Context, actorFP string) ([]dbgen.PrFile, error) {
	return s.q.ListPRFilesByActor(ctx, actorFP)
}

// BranchComparisonsByActor returns every cached branch comparison for one actor.
func (s *Store) BranchComparisonsByActor(ctx context.Context, actorFP string) ([]dbgen.BranchComparison, error) {
	return s.q.ListBranchComparisonsByActor(ctx, actorFP)
}

// CommitChecksByActor returns every cached per-check state row for one actor.
func (s *Store) CommitChecksByActor(ctx context.Context, actorFP string) ([]dbgen.CommitCheck, error) {
	return s.q.ListCommitChecksByActor(ctx, actorFP)
}

// DistinctOwnersByActor returns the distinct repo owners cached for one actor.
func (s *Store) DistinctOwnersByActor(ctx context.Context, actorFP string) ([]string, error) {
	return s.q.ListDistinctOwnersByActor(ctx, actorFP)
}
