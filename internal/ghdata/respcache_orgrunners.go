package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// State for GET /orgs/{org}/actions/runners. Keyed by the bearer's
// fingerprint like hooks_cache -- an admin-scoped read GitHub sends no
// webhook for (no event announces a runner going online/offline/busy), so a
// global row would both leak admin-only data through the ordinary reveal
// gate and go stale with nothing to notice it. TTL is short and primary.
// Cached VERBATIM (identical-or-passthrough, see respcache_identity.go's file
// header) -- a runner's `labels` array and other fields are exactly the kind
// of thing a trim guesses wrong about with no consumer survey to check
// against.

// OrgRunnersCacheTTL is short: a runner's busy/status is a live value a
// scheduler provisions against, and no webhook shortens the wait.
const OrgRunnersCacheTTL = 30 * time.Second

// GetCachedOrgRunners returns one page's cached document, or (empty, false)
// on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedOrgRunners(ctx context.Context, tokenFP, org string, perPage, page int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetOrgRunnersCache(ctx, dbgen.GetOrgRunnersCacheParams{
		TokenFp: tokenFP, Org: org, PerPage: perPage, Page: page,
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
	_ = s.q.TouchOrgRunnersCache(ctx, dbgen.TouchOrgRunnersCacheParams{
		LastUsedAt: rfc3339(now), TokenFp: tokenFP, Org: org, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedOrgRunners stores one page, then prunes (expired rows + LRU beyond
// the cap).
func (s *Store) PutCachedOrgRunners(ctx context.Context, tokenFP, org string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertOrgRunnersCache(ctx, dbgen.UpsertOrgRunnersCacheParams{
		TokenFp: tokenFP, Org: org, PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredOrgRunnersCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneOrgRunnersCacheLRU(ctx, CacheMaxRows)
}
