package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// State for GET /app/installations (every installation of the calling App --

// AppInstallationsCacheTTL bounds a stale answer for a missed delivery.
const AppInstallationsCacheTTL = time.Hour

// GetCachedAppInstallations returns one page's cached document, or (empty,
// false) on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedAppInstallations(ctx context.Context, appKey string, perPage, page int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetAppInstallationsCache(ctx, dbgen.GetAppInstallationsCacheParams{
		AppKey: appKey, PerPage: perPage, Page: page,
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
	_ = s.q.TouchAppInstallationsCache(ctx, dbgen.TouchAppInstallationsCacheParams{
		LastUsedAt: rfc3339(now), AppKey: appKey, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedAppInstallations stores one page, then prunes (expired rows + LRU
// beyond the cap).
func (s *Store) PutCachedAppInstallations(ctx context.Context, appKey string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertAppInstallationsCache(ctx, dbgen.UpsertAppInstallationsCacheParams{
		AppKey: appKey, PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredAppInstallationsCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneAppInstallationsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateAppInstallationsForApp drops every cached page for one App --
func (s *Store) InvalidateAppInstallationsForApp(ctx context.Context, appKey string) error {
	return s.q.DeleteAppInstallationsCacheForApp(ctx, appKey)
}
