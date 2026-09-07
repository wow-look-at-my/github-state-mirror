package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached README read:
//
//	GET /repos/{owner}/{repo}/readme[/{dir}][?ref=]
//
// One row per (owner, repo, requested subtree, VERBATIM requested ref). The
// row holds the rendered document and the status it answers with, so a repo
// that has no README costs one upstream call rather than one per poll. WHO may
// read a row is the reveal layer's job (internal/api).

// ReadmeCacheTTL backstops a lost push delivery. A README changes only with a
// push, and every push flushes these rows, so the TTL is a safety net.
const ReadmeCacheTTL = 6 * time.Hour

// CachedReadme is one stored README answer.
type CachedReadme struct {
	Status int    // 200 or 404
	Doc    string // rendered trimmed document
}

// GetCachedReadme returns the stored README answer, or false on a miss (no
// row, or an expired one). A hit refreshes the row's LRU stamp.
func (s *Store) GetCachedReadme(ctx context.Context, owner, repo, dir, ref string, now time.Time) (CachedReadme, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetReadmeCache(ctx, dbgen.GetReadmeCacheParams{
		Owner: ownerKey, Repo: repoKey, Dir: dir, Ref: ref,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedReadme{}, false, nil
	}
	if err != nil {
		return CachedReadme{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedReadme{}, false, nil
	}
	_ = s.q.TouchReadmeCache(ctx, dbgen.TouchReadmeCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Dir: dir, Ref: ref,
	})
	return CachedReadme{Status: int(row.Status), Doc: row.Doc}, true, nil
}

// PutCachedReadme records one fetched README answer, then prunes the table
// (expired rows + LRU beyond the cap).
func (s *Store) PutCachedReadme(ctx context.Context, owner, repo, dir, ref string, c CachedReadme, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertReadmeCache(ctx, dbgen.UpsertReadmeCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Dir: dir, Ref: ref,
		Status: int64(c.Status), Doc: c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredReadmeCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneReadmeCacheLRU(ctx, CacheMaxRows)
}

// InvalidateReadmeCache drops a repo's cached README answers, every ref.
func (s *Store) InvalidateReadmeCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteReadmeCacheByRepo(ctx, dbgen.DeleteReadmeCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateReadmeForRef drops the rows a caller asked for under one ref
// spelling. A push names its ref, so this is the grain it flushes at.
func (s *Store) InvalidateReadmeForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteReadmeCacheForRef(ctx, dbgen.DeleteReadmeCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}
