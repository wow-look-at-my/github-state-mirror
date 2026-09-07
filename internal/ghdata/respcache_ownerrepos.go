package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the owner repo listings. Each answers "the repos of this owner
// that THIS token can see", so a row keys the bearer's fingerprint: a shared
// row would reveal a token's private repos to another token.

const (
	// Part of the key: a user listing omits an organization's own repos.
	OwnerReposScopeOrg  = "org"
	OwnerReposScopeUser = "user"

	// OwnerReposCacheTTL is the primary bound; a repository event flushes sooner.
	OwnerReposCacheTTL = 10 * time.Minute
)

// OwnerReposKey addresses a stored owner-repos answer.
type OwnerReposKey struct {
	TokenFP   string
	Scope     string // OwnerReposScope*
	Owner     string
	Sort      string
	Direction string
	PerPage   int64
	Page      int64
}

// CachedOwnerRepos is a stored owner-repos answer.
type CachedOwnerRepos struct {
	Status int
	Doc    string
}

func (k OwnerReposKey) norm() OwnerReposKey {
	k.Owner = NormalizeRepoKey(k.Owner)
	return k
}

// GetCachedOwnerRepos returns the stored answer, or false on a miss. A hit
// refreshes the row's LRU stamp.
func (s *Store) GetCachedOwnerRepos(ctx context.Context, k OwnerReposKey, now time.Time) (CachedOwnerRepos, bool, error) {
	k = k.norm()
	row, err := s.q.GetOwnerReposCache(ctx, dbgen.GetOwnerReposCacheParams{
		TokenFp: k.TokenFP, Scope: k.Scope, Owner: k.Owner,
		Sort: k.Sort, Direction: k.Direction, PerPage: k.PerPage, Page: k.Page,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedOwnerRepos{}, false, nil
	}
	if err != nil {
		return CachedOwnerRepos{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedOwnerRepos{}, false, nil
	}
	_ = s.q.TouchOwnerReposCache(ctx, dbgen.TouchOwnerReposCacheParams{
		LastUsedAt: rfc3339(now), TokenFp: k.TokenFP, Scope: k.Scope, Owner: k.Owner,
		Sort: k.Sort, Direction: k.Direction, PerPage: k.PerPage, Page: k.Page,
	})
	return CachedOwnerRepos{Status: int(row.Status), Doc: row.Doc}, true, nil
}

// PutCachedOwnerRepos records a fetched answer, then prunes expired rows and
// the LRU tail beyond the cap.
func (s *Store) PutCachedOwnerRepos(ctx context.Context, k OwnerReposKey, c CachedOwnerRepos, now time.Time, ttl time.Duration) error {
	k = k.norm()
	if err := s.q.UpsertOwnerReposCache(ctx, dbgen.UpsertOwnerReposCacheParams{
		TokenFp: k.TokenFP, Scope: k.Scope, Owner: k.Owner,
		Sort: k.Sort, Direction: k.Direction, PerPage: k.PerPage, Page: k.Page,
		Status: int64(c.Status), Doc: c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredOwnerReposCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneOwnerReposCacheLRU(ctx, CacheMaxRows)
}

// InvalidateOwnerReposCache drops an owner's listings across every credential:
// a created or renamed repo moves all of them, and the delivery names no token.
func (s *Store) InvalidateOwnerReposCache(ctx context.Context, owner string) error {
	return s.q.DeleteOwnerReposCacheByOwner(ctx, NormalizeRepoKey(owner))
}
