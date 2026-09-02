package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// The storage layer for the cached compare route (.../compare/{basehead}); see docs/cache/rest-routes.md.

// CachedCompare is cached comparison: the rendered document exactly as
// the API layer will serve it. BaseRef/HeadRef are the basehead's sides
// (the API layer splits at the "..." its route guard already requires), kept
// as their own columns so a push to ref can flush exactly the
// comparisons naming it. Status is the upstream answer the row absorbed:
type CachedCompare struct {
	Owner    string // lowercased
	Repo     string // lowercased
	Basehead string // raw base...head path tail, exact
	BaseRef  string // basehead's base side (before the "...")
	HeadRef  string // basehead's head side (after the "...")
	// BaseTipSha is base_commit.sha; empty when unstated (or on a verdict row).
	BaseTipSha string
	Status     int    // , or (unknown-ref miss marker)
	Doc        string // rendered document as JSON (trimmed compare, or the body)
}

// GetCachedCompare returns the cached comparison, or (, false) on a miss
// (no row, or an expired). A hit refreshes the row's LRU timestamp.
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
	if moved, err := s.baseBranchMoved(ctx, ownerKey, repoKey, row.BaseRef, row.BaseTipSha, now); err != nil {
		return CachedCompare{}, false, err
	} else if moved {
		return CachedCompare{}, false, nil
	}
	_ = s.q.TouchCompareCache(ctx, dbgen.TouchCompareCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Basehead: basehead,
	})
	return CachedCompare{
		Owner: row.Owner, Repo: row.Repo, Basehead: row.Basehead,
		BaseRef: row.BaseRef, HeadRef: row.HeadRef, BaseTipSha: row.BaseTipSha,
		Status: int(row.Status), Doc: row.Doc,
	}, true, nil
}

// baseBranchMoved proves a compare row stale by tip comparison; see docs/cache/stale-tip-repair.md.
func (s *Store) baseBranchMoved(ctx context.Context, owner, repo, baseRef, baseTipSha string, now time.Time) (bool, error) {
	if baseTipSha == "" || IsFullHexSHA(baseRef) {
		return false, nil
	}
	tip, known, err := s.KnownBranchTip(ctx, owner, repo, baseRef, now)
	if err != nil || !known {
		return false, err
	}
	return tip != baseTipSha, nil
}

// PutCachedCompare absorbs comparison plus its commits, in transaction; c and commits must carry normalized owner/repo and lowercased shas.
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
		BaseRef: c.BaseRef, HeadRef: c.HeadRef, BaseTipSha: c.BaseTipSha,
		Status:    int64(c.Status),
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

// InvalidateCompareCache drops every cached comparison for a repo; see docs/cache/rest-routes.md.
func (s *Store) InvalidateCompareCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCompareCacheByRepo(ctx, dbgen.DeleteCompareCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateCompareForRef drops every cached comparison naming ref on either side; see docs/cache/rest-routes.md.
func (s *Store) InvalidateCompareForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteCompareCacheForRef(ctx, dbgen.DeleteCompareCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		BaseRef: ref, HeadRef: ref,
	})
}
