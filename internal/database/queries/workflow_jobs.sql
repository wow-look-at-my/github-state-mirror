-- ============================================================================
-- Workflow jobs (webhook-fed Actions job state; global, not actor-scoped)
-- ============================================================================
--
-- NOTE: keep these comments pure ASCII. sqlc v1.28.0's SQLite codegen slices
-- the generated query string with byte-vs-rune offset skew, so every multi-byte
-- character in a query's attached comment silently truncates the tail of the
-- generated SQL (e.g. one em dash eats the trailing "?" placeholder).

-- UpsertWorkflowJob records a workflow_job webhook's state, tolerating
-- out-of-order delivery: a completed event arriving before its in_progress
-- still inserts the full row, and a late in_progress for an already-completed
-- job must NOT regress status/conclusion/completed_at (the CASE guards). The
-- identity fields (run, attempt, name, sha, branch, url) are refreshed from
-- whichever event arrives; started_at and runner_name keep a known value when
-- the incoming payload doesn't carry one.
-- name: UpsertWorkflowJob :exec
INSERT INTO workflow_jobs (owner, repo, job_id, run_id, run_attempt, name, workflow_name,
                           status, conclusion, head_sha, head_branch, html_url,
                           started_at, completed_at, runner_name, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, job_id) DO UPDATE SET
    run_id        = excluded.run_id,
    run_attempt   = excluded.run_attempt,
    name          = excluded.name,
    workflow_name = excluded.workflow_name,
    head_sha      = excluded.head_sha,
    head_branch   = excluded.head_branch,
    html_url      = excluded.html_url,
    status        = CASE WHEN workflow_jobs.status = 'completed' AND excluded.status != 'completed'
                         THEN workflow_jobs.status ELSE excluded.status END,
    conclusion    = CASE WHEN workflow_jobs.status = 'completed' AND excluded.status != 'completed'
                         THEN workflow_jobs.conclusion ELSE excluded.conclusion END,
    completed_at  = CASE WHEN workflow_jobs.status = 'completed' AND excluded.status != 'completed'
                         THEN workflow_jobs.completed_at ELSE excluded.completed_at END,
    started_at    = CASE WHEN excluded.started_at = ''
                         THEN workflow_jobs.started_at ELSE excluded.started_at END,
    runner_name   = CASE WHEN excluded.runner_name = ''
                         THEN workflow_jobs.runner_name ELSE excluded.runner_name END,
    updated_at    = excluded.updated_at;

-- ListRecentWorkflowJobs returns running jobs first (newest started first),
-- then completed jobs (newest completed first). Timestamps are RFC3339 UTC, so
-- lexicographic order is chronological order.
-- name: ListRecentWorkflowJobs :many
SELECT * FROM workflow_jobs
ORDER BY
    CASE WHEN status = 'completed' THEN 1 ELSE 0 END,
    CASE WHEN status = 'completed' THEN completed_at ELSE started_at END DESC
LIMIT ?;

-- PruneWorkflowJobs deletes completed jobs that are stale on BOTH clocks:
-- completed_at (the job's own event time) AND updated_at (when the mirror
-- last applied a webhook for it) are older than the cutoff (RFC3339; both
-- placeholders receive the same value). Keying on updated_at is load-bearing
-- for UpsertWorkflowJob's out-of-order guard: the guard's only memory is the
-- row, and a prune keyed on completed_at alone deleted a just-recorded
-- completed row whose event time was already past the horizon (a replayed old
-- delivery; also the 2026-07-17 CI incident, where aged test fixtures crossed
-- it), letting a late in_progress INSERT a fresh running row -- status
-- regressed, conclusion blanked, and the phantom row never pruned. With the
-- updated_at key the row survives a full retention window after the last
-- webhook touched it, longer than any real redelivery arrives. Jobs still in
-- progress are never pruned here; they age out once their completed event
-- lands (a stuck row whose completed event was lost is bounded by CI volume).
-- The partial idx_workflow_jobs_completed_at index still narrows the scan to
-- completed rows in the stale completed_at range; updated_at only filters
-- that already-stale set further.
-- name: PruneWorkflowJobs :exec
DELETE FROM workflow_jobs
WHERE status = 'completed' AND completed_at < ? AND updated_at < ?;
