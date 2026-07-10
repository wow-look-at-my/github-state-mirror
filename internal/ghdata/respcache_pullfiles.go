package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached PR-files route:
//
//	GET /repos/{owner}/{repo}/pulls/{number}/files
//
// A pull_files_cache row stores the ALREADY-TRIMMED files array as one JSON
// blob, keyed by the exact request (owner, repo, number, per_page, page) --
// one self-contained answer per page, like the compare doc. A PR's files move
// whenever its head or base moves, so pull_request events flush that one PR's
// pages (head pushes -- including fork heads whose pushes we never see --
// base retargets, reopens) and push/repository events flush the whole repo
// (the belt for missed pull_request deliveries); expires_at is the 24h TTL
// backstop. WHO may read a cached page is the reveal layer's job
// (internal/api).

// GetCachedPullFiles returns the cached trimmed files-page document, or
// ("", false) on a miss (no row, or an expired one). A hit refreshes the
// row's LRU timestamp.
func (s *Store) GetCachedPullFiles(ctx context.Context, owner, repo string, number, perPage, page int64, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetPullFilesCache(ctx, dbgen.GetPullFilesCacheParams{
		Owner: ownerKey, Repo: repoKey, Number: number, PerPage: perPage, Page: page,
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
	_ = s.q.TouchPullFilesCache(ctx, dbgen.TouchPullFilesCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Number: number, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedPullFiles records one fetched files page, then prunes the table
// (expired rows + LRU beyond the cap). owner/repo are normalized here so
// callers can pass URL casing.
func (s *Store) PutCachedPullFiles(ctx context.Context, owner, repo string, number, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertPullFilesCache(ctx, dbgen.UpsertPullFilesCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		Number: number, PerPage: perPage, Page: page,
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredPullFilesCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PrunePullFilesCacheLRU(ctx, CacheMaxRows)
}

// InvalidatePullFilesCache drops every cached files page for a repo -- the
// push/repository webhook flush (a push may have moved any same-repo PR's
// head; the belt for missed pull_request deliveries). owner/repo are
// normalized here so callers can pass payload casing.
func (s *Store) InvalidatePullFilesCache(ctx context.Context, owner, repo string) error {
	return s.q.DeletePullFilesCacheByRepo(ctx, dbgen.DeletePullFilesCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidatePullFilesForPR drops one PR's cached files pages -- the
// pull_request event flush (head pushed/synchronize -- including fork heads
// whose pushes we never see -- base retargets, reopens). owner/repo are
// normalized here so callers can pass payload casing.
func (s *Store) InvalidatePullFilesForPR(ctx context.Context, owner, repo string, number int64) error {
	return s.q.DeletePullFilesCacheByPR(ctx, dbgen.DeletePullFilesCacheByPRParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Number: number,
	})
}
