package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the webhook CONFIGURATION listings:
//
//	GET /repos/{owner}/{repo}/hooks  (HooksScopeRepo)

// Hook listing scopes. The scope is part of the key so a repo named like an
// org can never answer the other question.
const (
	HooksScopeRepo = "repo"
	HooksScopeOrg  = "org"
)

// HooksTarget names one hook listing's subject: a repository, or an
// organization (Repo empty).
type HooksTarget struct {
	Scope string
	Owner string
	Repo  string
}

// RepoHooksTarget names a repository's hook listing.
func RepoHooksTarget(owner, repo string) HooksTarget {
	return HooksTarget{Scope: HooksScopeRepo, Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo)}
}

// OrgHooksTarget names an organization's hook listing.
func OrgHooksTarget(org string) HooksTarget {
	return HooksTarget{Scope: HooksScopeOrg, Owner: NormalizeRepoKey(org)}
}

// GetCachedHooks returns the stored listing, or ("", false) on a miss (no row,
// or an expired one). A hit refreshes the row's LRU stamp.
func (s *Store) GetCachedHooks(ctx context.Context, tokenFP string, t HooksTarget, perPage, page int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetHooksCache(ctx, dbgen.GetHooksCacheParams{
		TokenFp: tokenFP, Scope: t.Scope, Owner: t.Owner, Repo: t.Repo, PerPage: perPage, Page: page,
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
	_ = s.q.TouchHooksCache(ctx, dbgen.TouchHooksCacheParams{
		LastUsedAt: rfc3339(now), TokenFp: tokenFP, Scope: t.Scope, Owner: t.Owner, Repo: t.Repo,
		PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedHooks records one fetched listing page, then prunes the table
// (expired rows + LRU beyond the cap).
func (s *Store) PutCachedHooks(ctx context.Context, tokenFP string, t HooksTarget, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertHooksCache(ctx, dbgen.UpsertHooksCacheParams{
		TokenFp: tokenFP, Scope: t.Scope, Owner: t.Owner, Repo: t.Repo,
		PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredHooksCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneHooksCacheLRU(ctx, CacheMaxRows)
}

// InvalidateHooksForTarget drops one target's listings across EVERY
// credential. Rows are per-credential but a hook change is not: a hook created
// through the mirror by one caller changes the answer every other caller gets.
func (s *Store) InvalidateHooksForTarget(ctx context.Context, t HooksTarget) error {
	return s.q.DeleteHooksCacheForTarget(ctx, dbgen.DeleteHooksCacheForTargetParams{
		Scope: t.Scope, Owner: t.Owner, Repo: t.Repo,
	})
}
