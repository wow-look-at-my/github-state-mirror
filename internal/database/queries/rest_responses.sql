-- name: UpsertRESTResponse :exec
INSERT INTO rest_responses (actor, resource_kind, resource_key, status_code, content_type, body, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, resource_kind, resource_key) DO UPDATE SET
    status_code = excluded.status_code,
    content_type = excluded.content_type,
    body = excluded.body,
    updated_at = excluded.updated_at;

-- name: GetRESTResponse :one
SELECT * FROM rest_responses
WHERE actor = ? AND resource_kind = ? AND resource_key = ?;

-- name: MarkRESTResponsesStaleByKeyPrefix :exec
UPDATE cache_metadata
SET fetch_state = 'stale'
WHERE resource_kind = @resource_kind AND substr(resource_key, 1, length(@key_prefix)) = @key_prefix;
