package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached Code Quality setup read:
//
//	GET /repos/{owner}/{repo}/code-quality/setup
//
//  row per repo -- the endpoint takes no query parameters, so the repo is
// the whole key. The stored document is already trimmed and rendered. WHO may
// read it is the reveal layer's job (internal/api).

// GetCachedCodeQualitySetup returns the stored document, or ("", false) on a
// miss (no row, or an expired). A hit refreshes the row's LRU stamp.
func (s *Store) GetCachedCodeQualitySetup(ctx context.Context, owner, repo string, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetCodeQualitySetupCache(ctx, dbgen.GetCodeQualitySetupCacheParams{Owner: ownerKey, Repo: repoKey})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return "", false, nil
	}
	_ = s.q.TouchCodeQualitySetupCache(ctx, dbgen.TouchCodeQualitySetupCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey,
	})
	return row.Doc, true, nil
}

// PutCachedCodeQualitySetup records fetched configuration, then prunes the
// table (expired rows + LRU beyond the cap).
func (s *Store) PutCachedCodeQualitySetup(ctx context.Context, owner, repo, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertCodeQualitySetupCache(ctx, dbgen.UpsertCodeQualitySetupCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredCodeQualitySetupCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneCodeQualitySetupCacheLRU(ctx, CacheMaxRows)
}

// InvalidateCodeQualitySetup drops a repo's cached configuration: the flush
// for a PATCH the mirror proxies (the only change signal it can see) and for
// `repository` events.
func (s *Store) InvalidateCodeQualitySetup(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCodeQualitySetupCacheByRepo(ctx, dbgen.DeleteCodeQualitySetupCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
