-- ============================================================================
-- Actor Identities (dashboard only)
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
-- Cross-actor cache statistics (dashboard only)
-- ============================================================================

-- ListCachedActors returns every distinct actor that has any cache metadata,
-- so the admin view can attribute even scopes with no identity row (e.g. the
-- background service token before it is recorded).
-- name: ListCachedActors :many
SELECT DISTINCT actor FROM cache_metadata ORDER BY actor;

-- Per-table cached-row counts for one actor. Kept as separate single-table
-- queries (rather than one multi-table statement) because sqlc's SQLite analyzer
-- treats the shared `actor` column name across tables as ambiguous otherwise.
-- name: CountReposByActor :one
SELECT COUNT(*) FROM repos WHERE actor = ?;

-- name: CountPullRequestsByActor :one
SELECT COUNT(*) FROM pull_requests WHERE actor = ?;

-- name: CountOrgsByActor :one
SELECT COUNT(*) FROM orgs WHERE actor = ?;

-- name: CountUsersByActor :one
SELECT COUNT(*) FROM users WHERE actor = ?;

-- name: CountCommitChecksByActor :one
SELECT COUNT(*) FROM commit_checks WHERE actor = ?;

-- name: CountPRFilesByActor :one
SELECT COUNT(*) FROM pr_files WHERE actor = ?;

-- name: CountBranchComparisonsByActor :one
SELECT COUNT(*) FROM branch_comparisons WHERE actor = ?;

-- ActorFreshnessByKind summarizes cache_metadata for one actor, grouped by
-- resource kind and fetch state, with the most recent fetch time per group.
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

-- ActorRecentRefreshes returns the most recent refresh-log entries for one actor.
-- name: ActorRecentRefreshes :many
SELECT * FROM cache_refresh_log
WHERE actor = ?
ORDER BY id DESC
LIMIT ?;
