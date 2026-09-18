package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the README read: a row per (owner, repo, subtree, VERBATIM ref).

// ReadmeCacheTTL backstops a lost push; a push flushes these rows.
const ReadmeCacheTTL = 6 * time.Hour

// CachedReadme is a stored README answer.
type CachedReadme struct {
	Status int
	Doc    string
}

// GetCachedReadme returns the stored answer, or false on a miss. A hit
// refreshes the row's LRU stamp.
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

// PutCachedReadme records a fetched answer, then prunes expired rows and the
// LRU tail beyond the cap.
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

// InvalidateReadmeForRef drops the rows asked for under a ref spelling, the
// grain a push names.
func (s *Store) InvalidateReadmeForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteReadmeCacheForRef(ctx, dbgen.DeleteReadmeCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}
