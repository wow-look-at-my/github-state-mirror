package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached compare route:
//
//	GET /repos/{owner}/{repo}/compare/{basehead}
//
// A compare_cache row stores the ALREADY-TRIMMED compare document as one JSON
// blob, keyed by the exact request (owner, repo, raw basehead path tail) --
// unlike the commits list there is no rows+snapshot split, because a
// comparison is one self-contained answer, not an ordering over shared rows.
// The compare's commits ARE still upserted into the global git_commits_cache
// on absorb (the same rows the single git-commit route and push payloads
// maintain -- pure synergy; a compare hit never depends on them). A
// comparison depends on both refs' tips, so push/repository webhooks flush
// ALL of a repo's rows; expires_at is the 24h TTL backstop. WHO may read a
// cached comparison is the reveal layer's job (internal/api).

// CachedCompare is one cached comparison: the trimmed document exactly as the
// API layer will serve it.
type CachedCompare struct {
	Owner    string // lowercased
	Repo     string // lowercased
	Basehead string // raw base...head path tail, exact
	Doc      string // trimmed compare document as JSON
}

// GetCachedCompare returns the cached comparison, or (zero, false) on a miss
// (no row, or an expired one). A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedCompare(ctx context.Context, owner, repo, basehead string, now time.Time) (CachedCompare, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetCompareCache(ctx, dbgen.GetCompareCacheParams{
		Owner: ownerKey, Repo: repoKey, Basehead: basehead,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedCompare{}, false, nil
	}
	if err != nil {
		return CachedCompare{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedCompare{}, false, nil
	}
	_ = s.q.TouchCompareCache(ctx, dbgen.TouchCompareCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Basehead: basehead,
	})
	return CachedCompare{
		Owner: row.Owner, Repo: row.Repo, Basehead: row.Basehead, Doc: row.Doc,
	}, true, nil
}

// PutCachedCompare absorbs one fetched comparison: the compare's commits are
// upserted into the global git_commits_cache (synergy with the single-commit
// and commits-list routes) and the trimmed document is recorded, all in one
// transaction, then both tables are pruned (expired rows + LRU beyond the
// cap). c and commits must carry normalized owner/repo and lowercased shas
// (the API layer's absorb does).
func (s *Store) PutCachedCompare(ctx context.Context, c CachedCompare, commits []CachedGitCommit, now time.Time, ttl time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	for _, commit := range commits {
		if err := s.upsertGitCommit(ctx, q, commit, now); err != nil {
			return err
		}
	}
	if err := q.UpsertCompareCache(ctx, dbgen.UpsertCompareCacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Basehead: c.Basehead,
		Doc:       c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := s.q.DeleteExpiredCompareCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	if err := s.q.PruneCompareCacheLRU(ctx, CacheMaxRows); err != nil {
		return err
	}
	return s.q.PruneGitCommitsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateCompareCache drops every cached comparison for a repo -- the
// push/repository webhook flush (a push to either side of any basehead can
// change the comparison, so the whole repo flushes). owner/repo are
// normalized here so callers can pass payload casing.
func (s *Store) InvalidateCompareCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCompareCacheByRepo(ctx, dbgen.DeleteCompareCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
