package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the single-PR route's CLOSED answers:
//
//	GET /repos/{owner}/{repo}/pulls/{number}  (state closed/merged)
//
// A closed_pull_cache row stores the ALREADY-TRIMMED single-PR document as
// one JSON blob, rendered once at absorb time from GitHub's own response and
// never re-derived. The open-only invariant of the pull_requests truth table
// is untouched: a fetched closed PR still deletes any cached open row, and
// closed PRs live ONLY here, as rendered docs. A closed PR only changes via
// pull_request events (reopened/edited/relabeled), which flush that one PR's
// doc; repository events flush the whole repo; a push is deliberately NOT a
// flush -- it cannot mutate a closed PR. expires_at is the 24h TTL backstop
// for missed deliveries, the same accepted staleness class as PRRowFresh.
// WHO may read a cached doc is the reveal layer's job (internal/api).

// GetCachedClosedPull returns the cached trimmed closed-PR document, or
// ("", false) on a miss (no row, or an expired one). A hit refreshes the
// row's LRU timestamp.
func (s *Store) GetCachedClosedPull(ctx context.Context, owner, repo string, number int64, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetClosedPullCache(ctx, dbgen.GetClosedPullCacheParams{
		Owner: ownerKey, Repo: repoKey, Number: number,
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
	_ = s.q.TouchClosedPullCache(ctx, dbgen.TouchClosedPullCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Number: number,
	})
	return row.Doc, true, nil
}

// PutCachedClosedPull records one fetched closed-PR document, then prunes
// the table (expired rows + LRU beyond the cap). owner/repo are normalized
// here so callers can pass URL casing.
func (s *Store) PutCachedClosedPull(ctx context.Context, owner, repo string, number int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertClosedPullCache(ctx, dbgen.UpsertClosedPullCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Number: number,
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredClosedPullCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneClosedPullCacheLRU(ctx, CacheMaxRows)
}

// InvalidateClosedPullCache drops every cached closed-PR doc for a repo --
// the repository webhook flush. A push deliberately does not reach here: it
// cannot mutate a closed PR. owner/repo are normalized here so callers can
// pass payload casing.
func (s *Store) InvalidateClosedPullCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteClosedPullCacheByRepo(ctx, dbgen.DeleteClosedPullCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateClosedPullForPR drops one PR's cached closed doc -- the
// pull_request event flush (reopened/edited/relabeled; a close absorbs fresh
// on the next read), and the reopened-race safety after an open absorb.
// owner/repo are normalized here so callers can pass payload casing.
func (s *Store) InvalidateClosedPullForPR(ctx context.Context, owner, repo string, number int64) error {
	return s.q.DeleteClosedPullCacheByPR(ctx, dbgen.DeleteClosedPullCacheByPRParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Number: number,
	})
}
