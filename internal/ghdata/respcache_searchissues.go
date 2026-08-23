package ghdata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// State for GET /search/issues?q=...&per_page=...&page=... -- the
// documented exception to webhook-driven cache maintenance (see
// docs/cache/uncacheable-routes.md): an arbitrary search query names no
// single resource a webhook payload could patch, so this is TTL-only, and
// the TTL is short by design -- long enough to collapse a tight poll loop
// asking the identical query, short enough that a real answer change (an
// issue closed, a label added) is visible within one poll cycle either way.
// Cached VERBATIM (identical-or-passthrough, see respcache_identity.go's
// file header): a search result's `items` objects are exactly the kind of
// wide, unsurveyed shape a trim guesses wrong about.

// SearchIssuesCacheTTL is short: no webhook narrows the staleness window for
// an arbitrary query, so the TTL alone bounds it.
const SearchIssuesCacheTTL = 30 * time.Second

// SearchIssuesQueryKey hashes the modeled shape (q, per_page, page) into the
// cache key. The query string itself is unbounded, so it never appears as a
// SQL parameter directly.
func SearchIssuesQueryKey(q string, perPage, page int64) string {
	h := sha256.New()
	h.Write([]byte(q))
	h.Write([]byte{'\n'})
	h.Write([]byte(strconv.FormatInt(perPage, 10)))
	h.Write([]byte{'\n'})
	h.Write([]byte(strconv.FormatInt(page, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// GetCachedSearchIssues returns the cached document for (tokenFP, queryKey),
// or (empty, false) on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedSearchIssues(ctx context.Context, tokenFP, queryKey string, now time.Time) (string, bool, error) {
	row, err := s.q.GetSearchIssuesCache(ctx, dbgen.GetSearchIssuesCacheParams{
		TokenFp: tokenFP, QueryKey: queryKey,
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
	_ = s.q.TouchSearchIssuesCache(ctx, dbgen.TouchSearchIssuesCacheParams{
		LastUsedAt: rfc3339(now), TokenFp: tokenFP, QueryKey: queryKey,
	})
	return row.Doc, true, nil
}

// PutCachedSearchIssues stores one query's result, then prunes (expired rows
// + LRU beyond the cap).
func (s *Store) PutCachedSearchIssues(ctx context.Context, tokenFP, queryKey, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertSearchIssuesCache(ctx, dbgen.UpsertSearchIssuesCacheParams{
		TokenFp: tokenFP, QueryKey: queryKey, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredSearchIssuesCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneSearchIssuesCacheLRU(ctx, CacheMaxRows)
}
