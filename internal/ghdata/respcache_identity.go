package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// The identity/self routes (tier 2 of the cache contract):
//
//	GET /app          -- the calling App's own metadata (JWT-verified)
//	GET /user          -- the calling token's own GitHub user profile
//	GET /user/orgs     -- the calling token's own org memberships
//
// None of these has a per-repo scope, and none has a webhook signal: App
// metadata carries no delivery at all, and GitHub sends nothing for a user's
// own profile edits or org membership changes (the same gap CLAUDE.md already
// names as the `organization`/`membership` payload-unused exception). TTL is
// therefore the PRIMARY bound, the hooks_cache precedent, not a backstop.

// IdentityCacheTTL bounds how long a stale identity answer may be served.
const IdentityCacheTTL = 30 * time.Minute

const (
	IdentityKindApp      = "app"
	IdentityKindUser     = "user"
	IdentityKindUserOrgs = "user_orgs"
)

// GetCachedIdentity returns the cached document for (subjectKey, kind), or
// (empty, false) on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedIdentity(ctx context.Context, subjectKey, kind string, now time.Time) (string, bool, error) {
	row, err := s.q.GetIdentityCache(ctx, dbgen.GetIdentityCacheParams{SubjectKey: subjectKey, Kind: kind})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return "", false, nil
	}
	_ = s.q.TouchIdentityCache(ctx, dbgen.TouchIdentityCacheParams{LastUsedAt: rfc3339(now), SubjectKey: subjectKey, Kind: kind})
	return row.Doc, true, nil
}

// PutCachedIdentity stores one identity document, then prunes (expired rows +
// LRU beyond the cap).
func (s *Store) PutCachedIdentity(ctx context.Context, subjectKey, kind, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertIdentityCache(ctx, dbgen.UpsertIdentityCacheParams{
		SubjectKey: subjectKey, Kind: kind, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredIdentityCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneIdentityCacheLRU(ctx, CacheMaxRows)
}
