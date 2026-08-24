package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// State for GET /user/repos: the requesting token's own personalized repo
// listing, cached VERBATIM (identical-or-passthrough, see
// api/respcache_identity.go's file header). No webhook interaction -- see
// the schema comment for why; TTL alone bounds staleness.

// UserReposCacheTTL is short: a personalized cross-owner listing has no
// invalidation signal at all, so staleness is bounded purely by time.
const UserReposCacheTTL = 5 * time.Minute

// GetCachedUserRepos returns one page's cached document, or (empty, false)
// on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedUserRepos(ctx context.Context, tokenFP, sort string, perPage, page int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetUserReposCache(ctx, dbgen.GetUserReposCacheParams{
		TokenFp: tokenFP, Sort: sort, PerPage: perPage, Page: page,
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
	_ = s.q.TouchUserReposCache(ctx, dbgen.TouchUserReposCacheParams{
		LastUsedAt: rfc3339(now), TokenFp: tokenFP, Sort: sort, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedUserRepos stores one page, then prunes (expired rows + LRU beyond
// the cap).
func (s *Store) PutCachedUserRepos(ctx context.Context, tokenFP, sort string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertUserReposCache(ctx, dbgen.UpsertUserReposCacheParams{
		TokenFp: tokenFP, Sort: sort, PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredUserReposCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneUserReposCacheLRU(ctx, CacheMaxRows)
}
