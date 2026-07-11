package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the git-commit 404 miss markers:
//
//	GET /repos/{owner}/{repo}/git/commits/{sha} answering 404
//
// git_commits_cache never stores a 404 (a missing sha can be pushed later),
// which left one consumer pattern uncached forever: pr-minder's
// mergeWouldBeEmpty re-reads a GC'd test-merge sha on every fleet sweep, and
// each read was a fresh upstream 404. A git_commit_miss_cache row caches
// that verdict, bounded by expires_at, keyed (owner, repo, sha); doc holds
// the rendered 404 body. The un-miss path is the load-bearing part: EVERY
// real git-commit upsert clears the sha's marker (ghdata.upsertGitCommit --
// the single funnel all absorb paths share), so a sha that later
// materializes stops answering 404 immediately rather than waiting out the
// TTL. WHO may read a cached verdict is the reveal layer's job
// (internal/api).

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

// ClearGitCommitMiss drops one sha's 404 marker. The absorb paths run it via
// upsertGitCommit whenever a real commit is stored (the invariant that keeps
// a marker from shadowing a commit that now exists); it is also exported for
// the API layer's own direct use. owner/repo/sha are normalized here so
// callers can pass any casing.
func (s *Store) ClearGitCommitMiss(ctx context.Context, owner, repo, sha string) error {
	return s.q.DeleteGitCommitMiss(ctx, dbgen.DeleteGitCommitMissParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Sha: strings.ToLower(sha),
	})
}

// InvalidateGitCommitMissCache drops every 404 marker for a repo -- the
// repository webhook flush (a renamed/recreated repo's old verdicts must not
// keep answering). owner/repo are normalized here so callers can pass
// payload casing.
func (s *Store) InvalidateGitCommitMissCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteGitCommitMissCacheByRepo(ctx, dbgen.DeleteGitCommitMissCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
