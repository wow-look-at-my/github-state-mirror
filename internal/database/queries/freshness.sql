-- name: GetCacheMetadata :one
SELECT * FROM cache_metadata
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: UpsertCacheMetadata :exec
INSERT INTO cache_metadata (actor, resource_kind, resource_key, last_fetched_at, last_changed_at, etag, expires_at, fetch_state, error_message, retry_after)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, resource_kind, resource_key) DO UPDATE SET
    last_fetched_at = excluded.last_fetched_at,
    last_changed_at = excluded.last_changed_at,
    etag = excluded.etag,
    expires_at = excluded.expires_at,
    fetch_state = excluded.fetch_state,
    error_message = excluded.error_message,
    retry_after = excluded.retry_after;

-- name: MarkFetching :exec
UPDATE cache_metadata SET fetch_state = 'fetching'
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: MarkFresh :exec
UPDATE cache_metadata SET
    fetch_state = 'fresh',
    last_fetched_at = ?,
    etag = ?,
    expires_at = ?,
    error_message = NULL,
    retry_after = NULL
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: MarkStale :exec
UPDATE cache_metadata SET fetch_state = 'stale'
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: MarkStaleByKindKey :exec
UPDATE cache_metadata SET fetch_state = 'stale'
WHERE resource_kind = ? AND resource_key = ?;

-- name: MarkStaleByKindKeyPrefix :exec
UPDATE cache_metadata SET fetch_state = 'stale'
WHERE resource_kind = @resource_kind AND substr(resource_key, 1, length(@key_prefix)) = @key_prefix;

-- name: MarkError :exec
UPDATE cache_metadata SET
    fetch_state = 'error',
    error_message = ?,
    retry_after = ?
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: ListByKind :many
SELECT * FROM cache_metadata
WHERE actor = ? AND resource_kind = ?;

-- name: ListStale :many
SELECT * FROM cache_metadata
WHERE actor = ? AND (
    fetch_state IN ('stale', 'unknown')
    OR (fetch_state = 'fresh' AND expires_at < ?)
);

-- name: DeleteCacheMetadata :exec
DELETE FROM cache_metadata
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: InsertRefreshLog :one
INSERT INTO cache_refresh_log (actor, resource_kind, resource_key, triggered_by, started_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id;

-- name: CompleteRefreshLog :exec
UPDATE cache_refresh_log SET
    completed_at = ?,
    success = ?,
    records_changed = ?,
    error_message = ?
WHERE id = ?;
