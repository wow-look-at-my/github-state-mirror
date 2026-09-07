package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the owner repo listings:
//
//	GET /orgs/{org}/repos
//	GET /users/{username}/repos
//
// Both answer "the repos of this owner that THIS token can see", so a row
// keys the bearer's fingerprint alongside the owner. A shared row would reveal
// one token's private repos to another, which is why these listings do not
// join the global truth tables the reveal layer gates. The scope separates the
// two spellings: they answer differently for the same name, because the user
// listing of an organization omits the organization's own repos.

const (
	// OwnerReposScopeOrg and OwnerReposScopeUser are the two path spellings.
	OwnerReposScopeOrg  = "org"
	OwnerReposScopeUser = "user"

	// OwnerReposCacheTTL is the primary bound: a repository event flushes the
	// owner's rows, and the TTL covers a lost delivery.
	OwnerReposCacheTTL = 10 * time.Minute
)

// OwnerReposKey addresses one stored owner-repos answer.
type OwnerReposKey struct {
	TokenFP   string
	Scope     string // OwnerReposScope*
	Owner     string
	Sort      string
	Direction string
	PerPage   int64
	Page      int64
}

// CachedOwnerRepos is one stored owner-repos answer.
type CachedOwnerRepos struct {
	Status int    // 200 or 404
	Doc    string // rendered trimmed document
}

func (k OwnerReposKey) norm() OwnerReposKey {
	k.Owner = NormalizeRepoKey(k.Owner)
	return k
}

// GetCachedOwnerRepos returns the stored answer, or false on a miss (no row,
// or an expired one). A hit refreshes the row's LRU stamp.
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

// PutCachedOwnerRepos records one fetched answer, then prunes the table
// (expired rows + LRU beyond the cap).
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

// InvalidateOwnerReposCache drops an owner's cached listings across every
// credential: a created, deleted or renamed repo moves the listing for all of
// them, and there is no per-token signal in the delivery.
func (s *Store) InvalidateOwnerReposCache(ctx context.Context, owner string) error {
	return s.q.DeleteOwnerReposCacheByOwner(ctx, NormalizeRepoKey(owner))
}
