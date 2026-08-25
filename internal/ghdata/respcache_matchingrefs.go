package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// State for GET /repos/{owner}/{repo}/git/matching-refs/heads/{prefix}.

// MatchingRefsCacheTTL bounds a missed-delivery staleness window.
const MatchingRefsCacheTTL = 24 * time.Hour

// GetCachedMatchingRefs returns one page's cached document, or (empty, false)
// on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedMatchingRefs(ctx context.Context, owner, repo, prefix string, perPage, page int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetMatchingRefsCache(ctx, dbgen.GetMatchingRefsCacheParams{
		Owner: owner, Repo: repo, Prefix: prefix, PerPage: perPage, Page: page,
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
	_ = s.q.TouchMatchingRefsCache(ctx, dbgen.TouchMatchingRefsCacheParams{
		LastUsedAt: rfc3339(now), Owner: owner, Repo: repo, Prefix: prefix, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedMatchingRefs stores one page, then prunes (expired rows + LRU
// beyond the cap).
func (s *Store) PutCachedMatchingRefs(ctx context.Context, owner, repo, prefix string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertMatchingRefsCache(ctx, dbgen.UpsertMatchingRefsCacheParams{
		Owner: owner, Repo: repo, Prefix: prefix, PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredMatchingRefsCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneMatchingRefsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateMatchingRefsCache drops every cached prefix-search page for a
// repo.
func (s *Store) InvalidateMatchingRefsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteMatchingRefsCacheByRepo(ctx, dbgen.DeleteMatchingRefsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
