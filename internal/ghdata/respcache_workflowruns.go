package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached workflow-runs route:
//
//	GET /repos/{owner}/{repo}/actions/runs?head_sha=...
//
// A workflow_runs_cache row stores the ALREADY-TRIMMED runs document as one
// JSON blob, keyed by the exact request (owner, repo, head_sha, per_page,
// page) -- one self-contained answer per page, like the PR-files pages.
// pr-minder's hasWorkflowRuns zombie probe reads this listing per bot PR and
// the reconcile hook repeats it fleet-wide, so between CI events the answer
// is stable. A sha's runs change when its CI moves: status/check_run/
// check_suite/workflow_job webhooks flush that sha's rows (workflow_job's
// head_sha names the row directly) and repository events flush the whole
// repo; expires_at is the 24h TTL backstop. WHO may read a cached page is
// the reveal layer's job (internal/api).

// GetCachedWorkflowRuns returns the cached trimmed runs document for one
// exact pagination shape, or ("", false) on a miss (no row, or an expired
// one). A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedWorkflowRuns(ctx context.Context, owner, repo, headSHA string, perPage, page int, now time.Time) (string, bool, error) {
	ownerKey, repoKey, shaKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo), strings.ToLower(headSHA)
	row, err := s.q.GetWorkflowRunsCache(ctx, dbgen.GetWorkflowRunsCacheParams{
		Owner: ownerKey, Repo: repoKey, HeadSha: shaKey,
		PerPage: int64(perPage), Page: int64(page),
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
	_ = s.q.TouchWorkflowRunsCache(ctx, dbgen.TouchWorkflowRunsCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, HeadSha: shaKey,
		PerPage: int64(perPage), Page: int64(page),
	})
	return row.Doc, true, nil
}

// PutCachedWorkflowRuns records one fetched runs page, then prunes the table
// (expired rows + LRU beyond the cap). owner/repo/sha are normalized here so
// callers can pass URL casing.
func (s *Store) PutCachedWorkflowRuns(ctx context.Context, owner, repo, headSHA string, perPage, page int, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertWorkflowRunsCache(ctx, dbgen.UpsertWorkflowRunsCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), HeadSha: strings.ToLower(headSHA),
		PerPage: int64(perPage), Page: int64(page),
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredWorkflowRunsCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneWorkflowRunsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateWorkflowRunsCache drops every cached workflow-runs page for a
// repo -- the repository webhook flush, and the fallback when a CI/job
// payload carries no head sha. owner/repo are normalized here so callers can
// pass payload casing.
func (s *Store) InvalidateWorkflowRunsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteWorkflowRunsCacheByRepo(ctx, dbgen.DeleteWorkflowRunsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateWorkflowRunsForHeadSHA drops one sha's cached workflow-runs
// pages -- the per-sha status/check_run/check_suite/workflow_job flush (a
// new or finished run means the sha's listing may have changed). Other shas'
// pages survive. owner/repo/sha are normalized here so callers can pass
// payload casing.
func (s *Store) InvalidateWorkflowRunsForHeadSHA(ctx context.Context, owner, repo, headSHA string) error {
	return s.q.DeleteWorkflowRunsCacheForHeadSHA(ctx, dbgen.DeleteWorkflowRunsCacheForHeadSHAParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), HeadSha: strings.ToLower(headSHA),
	})
}
