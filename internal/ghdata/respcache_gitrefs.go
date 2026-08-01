package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached git-ref lookup:
//
//	GET /repos/{owner}/{repo}/git/ref/{ref}
//
// One row per (owner, repo, VERBATIM requested ref). The ref is never
// resolved or canonicalized: "main", "heads/main" and "refs/heads/main" are
// three distinct requests whose answers differ, so each keys its own row and
// the per-ref push flush covers every spelling (refSpellings). status carries
// what was absorbed -- 200 for a real ref, 404 for the absent-ref VERDICT --
// so the route can replay the answer under its own status. WHO may read a row
// is the reveal layer's job (internal/api).

// CachedGitRef is one stored ref answer.
type CachedGitRef struct {
	Owner  string
	Repo   string
	Ref    string
	Status int
	Doc    string
}

// GetCachedGitRef returns the cached ref answer, or ok=false on a miss (no
// row, or an expired one). A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedGitRef(ctx context.Context, owner, repo, ref string, now time.Time) (CachedGitRef, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetGitRefCache(ctx, dbgen.GetGitRefCacheParams{Owner: ownerKey, Repo: repoKey, Ref: ref})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedGitRef{}, false, nil
	}
	if err != nil {
		return CachedGitRef{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedGitRef{}, false, nil
	}
	_ = s.q.TouchGitRefCache(ctx, dbgen.TouchGitRefCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Ref: ref,
	})
	return CachedGitRef{Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Status: int(row.Status), Doc: row.Doc}, true, nil
}

// PutCachedGitRef records one fetched ref answer, then prunes the table
// (expired rows + LRU beyond the cap).
func (s *Store) PutCachedGitRef(ctx context.Context, c CachedGitRef, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertGitRefCache(ctx, dbgen.UpsertGitRefCacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Ref: c.Ref,
		Status: int64(c.Status), Doc: c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredGitRefCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneGitRefCacheLRU(ctx, CacheMaxRows)
}

// InvalidateGitRefForRef drops ONE requested spelling of a ref -- the per-ref
// push flush. A push moves, creates, or deletes exactly one ref, and creation
// is what clears a cached 404 verdict.
func (s *Store) InvalidateGitRefForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteGitRefCacheForRef(ctx, dbgen.DeleteGitRefCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}

// InvalidateGitRefCache drops every cached ref answer for a repo -- the
// repository-event flush (rename/delete/visibility) and the fallback for a
// push payload that names no usable ref.
func (s *Store) InvalidateGitRefCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteGitRefCacheByRepo(ctx, dbgen.DeleteGitRefCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
