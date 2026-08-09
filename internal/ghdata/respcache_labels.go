package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached single-label read:
//
//	GET /repos/{owner}/{repo}/labels/{name}
//
// One row per (owner, repo, VERBATIM requested name). The name is never
// canonicalized -- GitHub resolves label names case-insensitively but the
// mirror does not model that, so each requested spelling keys its own row and
// every flush is repo-wide, which covers all of them. WHO may read a row is
// the reveal layer's job (internal/api).

// GetCachedLabel returns the stored label document, or ("", false) on a miss
// (no row, or an expired one). A hit refreshes the row's LRU stamp.
func (s *Store) GetCachedLabel(ctx context.Context, owner, repo, name string, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetLabelCache(ctx, dbgen.GetLabelCacheParams{Owner: ownerKey, Repo: repoKey, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return "", false, nil
	}
	_ = s.q.TouchLabelCache(ctx, dbgen.TouchLabelCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Name: name,
	})
	return row.Doc, true, nil
}

// PutCachedLabel records one fetched label, then prunes the table (expired
// rows + LRU beyond the cap).
func (s *Store) PutCachedLabel(ctx context.Context, owner, repo, name, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertLabelCache(ctx, dbgen.UpsertLabelCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Name: name, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredLabelCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneLabelCacheLRU(ctx, CacheMaxRows)
}

// InvalidateLabelCache drops a repo's cached labels. Repo-wide is the only
// grain: a `label` rename moves two names at once, and one label answers under
// every spelling a caller might request.
func (s *Store) InvalidateLabelCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteLabelCacheByRepo(ctx, dbgen.DeleteLabelCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
