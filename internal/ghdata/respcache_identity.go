package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the identity/self routes (GET /app, /user, /user/orgs): TTL is the

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

// PutCachedIdentity stores identity document, then prunes (expired rows +
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
