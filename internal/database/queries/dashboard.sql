-- ============================================================================
-- Principal Identities (dashboard only)
-- ============================================================================

-- name: UpsertActorIdentity :exec
INSERT INTO actor_identities (actor, login, first_seen, last_seen)
VALUES (?, ?, ?, ?)
ON CONFLICT (actor) DO UPDATE SET
    login = excluded.login,
    last_seen = excluded.last_seen;

-- name: ListActorIdentities :many
SELECT * FROM actor_identities ORDER BY login, actor;

-- ============================================================================
-- Global cache statistics (dashboard only)
-- ============================================================================

-- ListKnownPrincipals returns every principal that holds freshness metadata,
-- so the admin view can attribute even principals with no identity row (e.g.
-- the background app-installation sessions). 'global' rows are truth
-- freshness, not a principal.
-- name: ListKnownPrincipals :many
SELECT DISTINCT actor FROM cache_metadata WHERE actor != 'global' ORDER BY actor;

-- Global (whole-truth-store) row counts. Kept as separate single-table
-- queries: sqlc's SQLite analyzer handles them more reliably than one
-- multi-table statement.
-- name: CountRepos :one
SELECT COUNT(*) FROM repos;

-- name: CountPullRequests :one
SELECT COUNT(*) FROM pull_requests;

-- name: CountCommitChecks :one
SELECT COUNT(*) FROM commit_checks;

-- name: CountContentsCache :one
SELECT COUNT(*) FROM contents_cache;

-- name: CountGitCommitsCache :one
SELECT COUNT(*) FROM git_commits_cache;

-- name: CountAccessGrants :one
SELECT COUNT(*) FROM access_grants;

-- ActorFreshnessByKind summarizes cache_metadata for one actor (a principal's
-- grant-sync markers, or 'global' truth markers), grouped by resource kind and
-- fetch state, with the most recent fetch time per group.
-- name: ActorFreshnessByKind :many
SELECT
    resource_kind,
    fetch_state,
    COUNT(*) AS count,
    MAX(last_fetched_at) AS last_fetched
FROM cache_metadata
WHERE actor = ?
GROUP BY resource_kind, fetch_state
ORDER BY resource_kind, fetch_state;

-- ActorErrorMessagesByKind returns the captured failure reason for every
-- resource currently in the 'error' state for one actor, so the dashboard can
-- show *why* a kind is erroring (not just that it is). One row per errored
-- resource key.
-- name: ActorErrorMessagesByKind :many
SELECT resource_kind, resource_key, error_message
FROM cache_metadata
WHERE actor = ? AND fetch_state = 'error' AND error_message IS NOT NULL AND error_message <> ''
ORDER BY resource_kind, resource_key;

-- ActorRecentRefreshes returns the most recent refresh-log entries for one actor.
-- name: ActorRecentRefreshes :many
SELECT * FROM cache_refresh_log
WHERE actor = ?
ORDER BY id DESC
LIMIT ?;

-- ============================================================================
-- Admin cache browse (dashboard, admin only)
-- ============================================================================
--
-- These dump the actual global-truth rows for the operator. Admin-gated in the
-- API layer; the consistency check compares them against GitHub.

-- name: ListAllRepos :many
SELECT * FROM repos ORDER BY owner, name;

-- name: ListAllPullRequests :many
SELECT * FROM pull_requests ORDER BY owner, repo, number;

-- ListAllOpenPullRequests returns only OPEN PRs. The consistency check compares
-- these against GitHub's live open-PR set (the cache only retains open PRs;
-- closed ones are deleted by the webhook dispatcher).
-- name: ListAllOpenPullRequests :many
SELECT * FROM pull_requests WHERE state = 'OPEN' ORDER BY owner, repo, number;

-- name: ListAllPRLabels :many
SELECT * FROM pr_labels ORDER BY owner, repo, pr_number, name;

-- name: ListAllCommitChecks :many
SELECT * FROM commit_checks ORDER BY owner, repo, sha, context;

-- name: ListDistinctRepoOwners :many
SELECT DISTINCT owner FROM repos ORDER BY owner;
