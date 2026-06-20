-- ============================================================================
-- Webhook delivery log (dashboard observability)
-- ============================================================================

-- name: InsertWebhookDelivery :exec
INSERT INTO webhook_deliveries (delivery_id, event_type, action, repo, received_at, disposition, detail, actors)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListRecentWebhookDeliveries :many
SELECT * FROM webhook_deliveries
ORDER BY id DESC
LIMIT ?;

-- PruneWebhookDeliveries keeps only the most recent rows. The subquery finds the
-- id of the (keep+1)th newest delivery; everything at or below it is deleted. If
-- there are fewer than keep+1 rows the subquery is NULL and nothing is removed.
-- name: PruneWebhookDeliveries :exec
DELETE FROM webhook_deliveries
WHERE id <= (
    SELECT id FROM webhook_deliveries
    ORDER BY id DESC
    LIMIT 1 OFFSET ?
);
