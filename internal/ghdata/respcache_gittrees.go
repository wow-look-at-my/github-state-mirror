package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Content-addressed and immutable; no TTL, no webhook invalidation, LRU-pruned.
// see docs/cache/rest-routes.md

// One absorbed tree, already rendered as trimmed JSON.
type CachedGitTree struct {
	Owner     string
	Repo      string
	SHA       string
	Recursive string // '' or '1'
	Doc       string
}

// GetCachedGitTree returns the cached tree document, or (zero, false) on a
// miss. Trees are immutable: no TTL check. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedGitTree(ctx context.Context, owner, repo, sha, recursive string, now time.Time) (CachedGitTree, bool, error) {
	row, err := s.q.GetGitTreeCache(ctx, dbgen.GetGitTreeCacheParams{
		Owner: owner, Repo: repo, Sha: sha, Recursive: recursive,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedGitTree{}, false, nil
	}
	if err != nil {
		return CachedGitTree{}, false, err
	}
	_ = s.q.TouchGitTreeCache(ctx, dbgen.TouchGitTreeCacheParams{
		LastUsedAt: rfc3339(now), Owner: owner, Repo: repo, Sha: sha, Recursive: recursive,
	})
	return CachedGitTree{Owner: row.Owner, Repo: row.Repo, SHA: row.Sha, Recursive: row.Recursive, Doc: row.Doc}, true, nil
}

// PutCachedGitTree stores one tree document, then prunes.
func (s *Store) PutCachedGitTree(ctx context.Context, c CachedGitTree, now time.Time) error {
	if err := s.q.UpsertGitTreeCache(ctx, dbgen.UpsertGitTreeCacheParams{
		Owner: c.Owner, Repo: c.Repo, Sha: c.SHA, Recursive: c.Recursive, Doc: c.Doc,
		FetchedAt: rfc3339(now), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	return s.q.PruneGitTreesCacheLRU(ctx, CacheMaxRows)
}
