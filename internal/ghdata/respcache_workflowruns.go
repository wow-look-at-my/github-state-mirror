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
//	GET /repos/{owner}/{repo}/actions/runs[?head_sha=|?status=&branch=]
//
// A workflow_runs_cache row stores the ALREADY-TRIMMED runs document as one
// JSON blob, keyed by the exact request (owner, repo, head_sha, filters,
// per_page, page) -- one self-contained answer per page, like the PR-files
// pages. Two request shapes share the table and are keyed apart: the
// per-COMMIT listing (head_sha set, filters '') that pr-minder's
// hasWorkflowRuns zombie probe reads per bot PR, and the repo-wide LISTING
// (head_sha '', filters set) that the GHA runner coordinator polls for its
// queued backlog.
//
// Invalidation differs by shape, which is why they are addressable apart. A
// sha's runs change when its CI moves, so status/check_run/check_suite/
// workflow_job webhooks flush that sha's rows (workflow_job's head_sha names
// the row directly). A LISTING names no sha at all -- any run in the repo
// entering or leaving `queued` changes it -- so the same deliveries flush
// every listing row for the repo. repository events flush both. expires_at
// is the backstop for run DELETION (which GitHub never delivers) and missed
// deliveries; internal/api chooses the per-shape TTL. WHO may read a cached
// page is the reveal layer's job (internal/api).

// GetCachedWorkflowRuns returns the cached trimmed runs document for one
// exact request shape, or ("", false) on a miss (no row, or an expired one).
// A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedWorkflowRuns(ctx context.Context, owner, repo, headSHA, filters string, perPage, page int, now time.Time) (string, bool, error) {
	ownerKey, repoKey, shaKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo), strings.ToLower(headSHA)
	row, err := s.q.GetWorkflowRunsCache(ctx, dbgen.GetWorkflowRunsCacheParams{
		Owner: ownerKey, Repo: repoKey, HeadSha: shaKey, Filters: filters,
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
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, HeadSha: shaKey, Filters: filters,
		PerPage: int64(perPage), Page: int64(page),
	})
	return row.Doc, true, nil
}

// PutCachedWorkflowRuns records one fetched runs page, then prunes the table
// (expired rows + LRU beyond the cap). owner/repo/sha are normalized here so
// callers can pass URL casing; filters arrives already canonical from the
// route's own query parse.
func (s *Store) PutCachedWorkflowRuns(ctx context.Context, owner, repo, headSHA, filters string, perPage, page int, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertWorkflowRunsCache(ctx, dbgen.UpsertWorkflowRunsCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), HeadSha: strings.ToLower(headSHA),
		Filters: filters,
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
// pages survive, as do the sha-less LISTING rows (those are
// InvalidateWorkflowRunsListings' job). owner/repo/sha are normalized here
// so callers can pass payload casing.
func (s *Store) InvalidateWorkflowRunsForHeadSHA(ctx context.Context, owner, repo, headSHA string) error {
	return s.q.DeleteWorkflowRunsCacheForHeadSHA(ctx, dbgen.DeleteWorkflowRunsCacheForHeadSHAParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), HeadSha: strings.ToLower(headSHA),
	})
}

// InvalidateWorkflowRunsListings drops a repo's sha-less LISTING snapshots
// (every filter, every page) while leaving the per-commit rows alone. This
// is the flush that makes the queued-backlog listing safe to store: it runs
// on EVERY run-state delivery for the repo, because a run entering or
// leaving `queued` changes an answer that names no sha for the per-sha flush
// to match. owner/repo are normalized here so callers can pass payload
// casing.
func (s *Store) InvalidateWorkflowRunsListings(ctx context.Context, owner, repo string) error {
	return s.q.DeleteWorkflowRunsCacheListings(ctx, dbgen.DeleteWorkflowRunsCacheListingsParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
