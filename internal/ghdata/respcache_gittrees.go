package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Git trees (GET /repos/{owner}/{repo}/git/trees/{sha}[?recursive=1]) are
// content-addressed and immutable, like git commits: no TTL, no webhook
// invalidation (no delivery names a tree object), LRU pruning only.
// recursive is part of the key ('' or '1', verbatim) because GitHub answers a
// DIFFERENT entry set for the same sha depending on it.

// CachedGitTree is one absorbed tree document, already rendered as trimmed
// JSON (the hooks_cache/branches_list_cache snapshot convention -- a tree has
// no other row that needs its individual fields).
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
