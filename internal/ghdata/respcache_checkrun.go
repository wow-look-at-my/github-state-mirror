package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// The single-check-run route (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/check-runs/{check_run_id}
//
// Distinct from commit_ci_cache's LIST route (GET .../commits/{ref}/check-runs):
// this table is keyed by the check run's OWN id, which never changes owner,
// repo, or sha once created. That makes a `check_run` delivery always
// directly appliable -- the payload carries the run's whole current state and
// this row's key IS that run's id, so there is no membership question the
// LIST route has to solve. TrimCheckRun (checkruns_doc.go) is the one trim
// both the fetch-on-miss path and the delivery rewrite use, so a stored
// answer never drifts from what a fresh fetch would produce.

// CheckRunCacheTTL bounds a stale answer for a check run whose terminal state
// no delivery ever reaches (a deleted App, an ordering loss); `check_run`
// deliveries rewrite the row far sooner in the common case.
const CheckRunCacheTTL = 24 * time.Hour

// GetCachedCheckRun returns the cached single-check-run document, or (empty,
// false) on a miss/expiry. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedCheckRun(ctx context.Context, owner, repo string, checkRunID int64, now time.Time) (string, bool, error) {
	row, err := s.q.GetCheckRunCache(ctx, dbgen.GetCheckRunCacheParams{
		Owner: owner, Repo: repo, CheckRunID: checkRunID,
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
	_ = s.q.TouchCheckRunCache(ctx, dbgen.TouchCheckRunCacheParams{
		LastUsedAt: rfc3339(now), Owner: owner, Repo: repo, CheckRunID: checkRunID,
	})
	return row.Doc, true, nil
}

// PutCachedCheckRun stores one check run's rendered document, then prunes
// (expired rows + LRU beyond the cap).
func (s *Store) PutCachedCheckRun(ctx context.Context, owner, repo string, checkRunID int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertCheckRunCache(ctx, dbgen.UpsertCheckRunCacheParams{
		Owner: owner, Repo: repo, CheckRunID: checkRunID, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredCheckRunCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneCheckRunCacheLRU(ctx, CacheMaxRows)
}

// ApplyCheckRunByID rewrites (or creates) the single-check-run row for a
// `check_run` delivery's own run object -- the whole point of keying this
// table by id: the row IS the answer for that id, so there is no
// "does the stored page still list it" question to solve first.
func (s *Store) ApplyCheckRunByID(ctx context.Context, owner, repo string, run StoredCheckRun, now time.Time, ttl time.Duration) error {
	if run.ID <= 0 {
		return nil
	}
	doc, err := MarshalCacheDoc(run)
	if err != nil {
		return err
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	return s.PutCachedCheckRun(ctx, ownerKey, repoKey, run.ID, string(doc), now, ttl)
}

// InvalidateCheckRunCacheByRepo drops every cached single-check-run row for a
// repo -- the repository-event/unparseable-payload backstop, the same
// conservative fallback commit_ci_cache uses.
func (s *Store) InvalidateCheckRunCacheByRepo(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCheckRunCacheByRepo(ctx, dbgen.DeleteCheckRunCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
