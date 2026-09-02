package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached Actions JOB reads (both run_jobs and job kinds share this row space).
// see docs/cache/rest-routes.md

// Kinds stored in workflow_jobs_cache.kind.
const (
	WorkflowJobsKindRunJobs = "run_jobs"
	WorkflowJobsKindJob     = "job"
)

// GetCachedWorkflowJobs returns the stored trimmed document, or ("", false)
// on a miss (no row, or an expired). A hit refreshes the row's LRU stamp.
func (s *Store) GetCachedWorkflowJobs(ctx context.Context, owner, repo, kind string, refID, perPage, page int64, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetWorkflowJobsCache(ctx, dbgen.GetWorkflowJobsCacheParams{
		Owner: ownerKey, Repo: repoKey, Kind: kind, RefID: refID, PerPage: perPage, Page: page,
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
	_ = s.q.TouchWorkflowJobsCache(ctx, dbgen.TouchWorkflowJobsCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey,
		Kind: kind, RefID: refID, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedWorkflowJobs records fetched answer, then prunes the table
// (expired rows + LRU beyond the cap).
func (s *Store) PutCachedWorkflowJobs(ctx context.Context, owner, repo, kind string, refID, runID, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertWorkflowJobsCache(ctx, dbgen.UpsertWorkflowJobsCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		Kind: kind, RefID: refID, RunID: runID, PerPage: perPage, Page: page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredWorkflowJobsCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneWorkflowJobsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateWorkflowJobsForRun drops everything a run's jobs back: its jobs
// pages AND the single-job rows under it. A re-run replaces a run's jobs
// under the same run id, which is exactly what this flush answers.
func (s *Store) InvalidateWorkflowJobsForRun(ctx context.Context, owner, repo string, runID int64) error {
	return s.q.DeleteWorkflowJobsCacheForRun(ctx, dbgen.DeleteWorkflowJobsCacheForRunParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), RunID: runID,
	})
}

// InvalidateWorkflowJobsCache drops a repo's cached job answers -- the
// repository-event flush, and the fallback when a payload names no run.
func (s *Store) InvalidateWorkflowJobsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteWorkflowJobsCacheByRepo(ctx, dbgen.DeleteWorkflowJobsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
