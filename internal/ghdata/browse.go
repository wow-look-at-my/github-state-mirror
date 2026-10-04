package ghdata

import (
	"context"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Admin browse over global truth; read-only, unfiltered by the reveal layer.

// AllRepos returns every repo row in global truth.
func (s *Store) AllRepos(ctx context.Context) ([]dbgen.Repo, error) {
	return s.qx(ctx).ListAllRepos(ctx)
}

// AllPullRequests returns every cached PR (any state).
func (s *Store) AllPullRequests(ctx context.Context) ([]dbgen.PullRequest, error) {
	return s.qx(ctx).ListAllPullRequests(ctx)
}

// AllOpenPullRequests returns only OPEN cached PRs (what the consistency check
// compares against GitHub's live open-PR set).
func (s *Store) AllOpenPullRequests(ctx context.Context) ([]dbgen.PullRequest, error) {
	return s.qx(ctx).ListAllOpenPullRequests(ctx)
}

// AllPRLabels returns every cached PR label.
func (s *Store) AllPRLabels(ctx context.Context) ([]dbgen.PrLabel, error) {
	return s.qx(ctx).ListAllPRLabels(ctx)
}

// AllCommitChecks returns every cached per-check state row.
func (s *Store) AllCommitChecks(ctx context.Context) ([]dbgen.CommitCheck, error) {
	return s.qx(ctx).ListAllCommitChecks(ctx)
}

// DistinctRepoOwners returns the distinct owners present in global truth.
func (s *Store) DistinctRepoOwners(ctx context.Context) ([]string, error) {
	return s.qx(ctx).ListDistinctRepoOwners(ctx)
}
