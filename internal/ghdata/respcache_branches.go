package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached branches route:
//
//	GET /repos/{owner}/{repo}/branches
//
// A branches_list_cache row stores the ALREADY-TRIMMED branches array as one
// JSON blob, keyed by the exact request (owner, repo, per_page, page) -- one
// self-contained answer per page, like the compare doc. A listing moves
// whenever any branch is created, deleted, or its tip advances, all of which
// arrive as push events, so push/repository webhooks flush a repo's
// snapshots; expires_at is the 24h TTL backstop for missed deliveries. WHO
// may read a cached page is the reveal layer's job (internal/api).

// GetCachedBranchesList returns the cached trimmed branches-page document, or
// ("", false) on a miss (no row, or an expired one). A hit refreshes the
// row's LRU timestamp.
func (s *Store) GetCachedBranchesList(ctx context.Context, owner, repo string, perPage, page int64, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetBranchesListCache(ctx, dbgen.GetBranchesListCacheParams{
		Owner: ownerKey, Repo: repoKey, PerPage: perPage, Page: page,
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
	_ = s.q.TouchBranchesListCache(ctx, dbgen.TouchBranchesListCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedBranchesList records one fetched branches page, then prunes the
// table (expired rows + LRU beyond the cap). owner/repo are normalized here
// so callers can pass URL casing.
func (s *Store) PutCachedBranchesList(ctx context.Context, owner, repo string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertBranchesListCache(ctx, dbgen.UpsertBranchesListCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		PerPage: perPage, Page: page,
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredBranchesListCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneBranchesListCacheLRU(ctx, CacheMaxRows)
}

// InvalidateBranchesListCache drops every cached branches page for a repo --
// the push/repository webhook flush (branch create, delete, and tip-move all
// arrive as pushes). owner/repo are normalized here so callers can pass
// payload casing.
func (s *Store) InvalidateBranchesListCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteBranchesListCacheByRepo(ctx, dbgen.DeleteBranchesListCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
