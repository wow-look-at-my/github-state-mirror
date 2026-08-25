package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached installation-repositories listing:
//
//	GET /installation/repositories
//
// One row per (credential fingerprint, per_page, page). The key is the
// CREDENTIAL because the answer is one installation token's own view -- see
// the schema comment for why the reveal-layer principal is the wrong key
// here. Only the fingerprint is stored; the bearer never is.

// GetCachedInstallationRepos returns the stored listing, or ("", false) on a
// miss (no row, or an expired one). A hit refreshes the row's LRU stamp.
func (s *Store) GetCachedInstallationRepos(ctx context.Context, tokenFP string, perPage, page int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetInstallationReposCache(ctx, dbgen.GetInstallationReposCacheParams{
		TokenFp: tokenFP, PerPage: perPage, Page: page,
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
	_ = s.q.TouchInstallationReposCache(ctx, dbgen.TouchInstallationReposCacheParams{
		LastUsedAt: rfc3339(now), TokenFp: tokenFP, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedInstallationRepos records one fetched listing page, then prunes the
// table (expired rows + LRU beyond the cap).
func (s *Store) PutCachedInstallationRepos(ctx context.Context, tokenFP string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertInstallationReposCache(ctx, dbgen.UpsertInstallationReposCacheParams{
		TokenFp: tokenFP, PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredInstallationReposCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneInstallationReposCacheLRU(ctx, CacheMaxRows)
}

// InvalidateInstallationRepos drops every cached listing: rows key a credential, not an installation id, so there is nothing finer to match on.
func (s *Store) InvalidateInstallationRepos(ctx context.Context) error {
	return s.q.DeleteAllInstallationReposCache(ctx)
}
