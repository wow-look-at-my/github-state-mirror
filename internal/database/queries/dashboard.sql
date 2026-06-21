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

-- ============================================================================
-- Admin cache browse (dashboard, admin only)
-- ============================================================================
--
-- These dump the actual cached rows for ONE explicit actor (cache scope). Unlike
-- the per-context reads in store.go (which take the actor from the request
-- context), these take the actor as a parameter so an admin can inspect any
-- scope. They are gated to admins in the API layer; the data tables remain keyed
-- by the opaque fingerprint, so this does not change the storage model -- it only
-- lets the operator read what is already there.

-- name: ListReposByActor :many
SELECT * FROM repos WHERE actor = ? ORDER BY owner, name;

-- name: ListPullRequestsByActor :many
SELECT * FROM pull_requests WHERE actor = ? ORDER BY owner, repo, number;

-- ListOpenPullRequestsByActor returns only OPEN PRs for an actor. The
-- consistency check compares these against GitHub's live open-PR set (the cache
-- only retains open PRs; closed ones are deleted by the webhook dispatcher).
-- name: ListOpenPullRequestsByActor :many
SELECT * FROM pull_requests WHERE actor = ? AND state = 'OPEN' ORDER BY owner, repo, number;

-- name: ListPRLabelsByActor :many
SELECT * FROM pr_labels WHERE actor = ? ORDER BY owner, repo, pr_number, name;

-- name: ListUsersByActor :many
SELECT * FROM users WHERE actor = ? ORDER BY login;

-- name: ListOrgsByActor :many
SELECT * FROM orgs WHERE actor = ? ORDER BY login;

-- name: ListPRFilesByActor :many
SELECT * FROM pr_files WHERE actor = ? ORDER BY owner, repo, pr_number, path;

-- name: ListBranchComparisonsByActor :many
SELECT * FROM branch_comparisons WHERE actor = ? ORDER BY owner, repo, base_ref, head_ref;

-- name: ListCommitChecksByActor :many
SELECT * FROM commit_checks WHERE actor = ? ORDER BY owner, repo, sha, context;

-- ListDistinctOwnersByActor returns the distinct repo owners cached for an actor,
-- so the consistency check knows which orgs to re-fetch from GitHub.
-- name: ListDistinctOwnersByActor :many
SELECT DISTINCT owner FROM repos WHERE actor = ? ORDER BY owner;

-- ActorErrorMessagesByKind returns the captured failure reason for every
-- resource currently in the 'error' state for one actor, so the dashboard can
-- show *why* a kind is erroring (not just that it is). One row per errored
-- resource key.
-- name: ActorErrorMessagesByKind :many
SELECT resource_kind, resource_key, error_message
FROM cache_metadata
WHERE actor = ? AND fetch_state = 'error' AND error_message IS NOT NULL AND error_message <> ''
ORDER BY resource_kind, resource_key;
