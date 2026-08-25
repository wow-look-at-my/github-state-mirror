package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage layer for the git-commit 404 miss markers (git_commit_miss_cache).
// See docs/cache/rest-routes.md for why they exist and the clear-on-upsert invariant.

// GetCachedGitCommitMiss returns the cached 404 body for a sha, or
// ("", false) on a miss (no marker, or an expired one). A hit refreshes the
// row's LRU timestamp.
func (s *Store) GetCachedGitCommitMiss(ctx context.Context, owner, repo, sha string, now time.Time) (string, bool, error) {
	ownerKey, repoKey, shaKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo), strings.ToLower(sha)
	row, err := s.q.GetGitCommitMissCache(ctx, dbgen.GetGitCommitMissCacheParams{
		Owner: ownerKey, Repo: repoKey, Sha: shaKey,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return "", false, nil
	}
	_ = s.q.TouchGitCommitMissCache(ctx, dbgen.TouchGitCommitMissCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Sha: shaKey,
	})
	return row.Doc, true, nil
}

// PutCachedGitCommitMiss records one fetched 404 verdict, then prunes the
// table (expired rows + LRU beyond the cap). owner/repo/sha are normalized
// here so callers can pass URL casing.
func (s *Store) PutCachedGitCommitMiss(ctx context.Context, owner, repo, sha, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertGitCommitMissCache(ctx, dbgen.UpsertGitCommitMissCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Sha: strings.ToLower(sha),
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredGitCommitMissCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneGitCommitMissCacheLRU(ctx, CacheMaxRows)
}

// ClearGitCommitMiss drops one sha's 404 marker; upsertGitCommit calls this on every real commit absorb.
func (s *Store) ClearGitCommitMiss(ctx context.Context, owner, repo, sha string) error {
	return s.q.DeleteGitCommitMiss(ctx, dbgen.DeleteGitCommitMissParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Sha: strings.ToLower(sha),
	})
}

// InvalidateGitCommitMissCache is the repository-event flush: a renamed/recreated repo's old verdicts must not keep answering.
func (s *Store) InvalidateGitCommitMissCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteGitCommitMissCacheByRepo(ctx, dbgen.DeleteGitCommitMissCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
