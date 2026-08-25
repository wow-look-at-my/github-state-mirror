package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the single-PR diff read's 406 verdicts (GET .../pulls/{number} with the diff media type).
// see docs/cache/rest-routes.md

// GetCachedPullDiff406 returns the cached 406 body for a PR, or ("", false)
// on a miss (no verdict, or an expired one). A hit refreshes the row's LRU
// timestamp.
func (s *Store) GetCachedPullDiff406(ctx context.Context, owner, repo string, number int64, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetPullDiff406Cache(ctx, dbgen.GetPullDiff406CacheParams{
		Owner: ownerKey, Repo: repoKey, Number: number,
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
	_ = s.q.TouchPullDiff406Cache(ctx, dbgen.TouchPullDiff406CacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Number: number,
	})
	return row.Doc, true, nil
}

// PutCachedPullDiff406 records one fetched 406 verdict, then prunes the
// table (expired rows + LRU beyond the cap). owner/repo are normalized here
// so callers can pass URL casing.
func (s *Store) PutCachedPullDiff406(ctx context.Context, owner, repo string, number int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertPullDiff406Cache(ctx, dbgen.UpsertPullDiff406CacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Number: number,
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredPullDiff406Cache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PrunePullDiff406CacheLRU(ctx, CacheMaxRows)
}

// InvalidatePullDiff406Cache drops every cached 406 verdict for a repo -- the push/repository flush (no per-PR signal for a base push moving the boundary).
func (s *Store) InvalidatePullDiff406Cache(ctx context.Context, owner, repo string) error {
	return s.q.DeletePullDiff406CacheByRepo(ctx, dbgen.DeletePullDiff406CacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidatePullDiff406ForPR drops one PR's cached 406 verdict -- the
// pull_request/pull_request_review event flush (a head push or retarget can
// shrink the diff back under the boundary). owner/repo are normalized here
// so callers can pass payload casing.
func (s *Store) InvalidatePullDiff406ForPR(ctx context.Context, owner, repo string, number int64) error {
	return s.q.DeletePullDiff406CacheForPR(ctx, dbgen.DeletePullDiff406CacheForPRParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Number: number,
	})
}
