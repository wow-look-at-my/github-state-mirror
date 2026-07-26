package ghdata

import (
	"context"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Admin cache browse + consistency-check reads over the GLOBAL truth store.
// Read-only; gated to admins in the API layer (the operator's dashboard is the
// one surface the reveal layer does not filter).

// AllRepos returns every repo row in global truth.
func (s *Store) AllRepos(ctx context.Context) ([]dbgen.Repo, error) {
	return s.q.ListAllRepos(ctx)
}

// AllPullRequests returns every cached PR (any state).
func (s *Store) AllPullRequests(ctx context.Context) ([]dbgen.PullRequest, error) {
	return s.q.ListAllPullRequests(ctx)
}

// AllOpenPullRequests returns only OPEN cached PRs (what the consistency check
// compares against GitHub's live open-PR set).
func (s *Store) AllOpenPullRequests(ctx context.Context) ([]dbgen.PullRequest, error) {
	return s.q.ListAllOpenPullRequests(ctx)
}

// AllPRLabels returns every cached PR label.
func (s *Store) AllPRLabels(ctx context.Context) ([]dbgen.PrLabel, error) {
	return s.q.ListAllPRLabels(ctx)
}

// AllCommitChecks returns every cached per-check state row.
func (s *Store) AllCommitChecks(ctx context.Context) ([]dbgen.CommitCheck, error) {
	return s.q.ListAllCommitChecks(ctx)
}

// DistinctRepoOwners returns the distinct owners present in global truth.
func (s *Store) DistinctRepoOwners(ctx context.Context) ([]string, error) {
	return s.q.ListDistinctRepoOwners(ctx)
}
