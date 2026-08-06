-- ============================================================================
-- Webhook delivery log (dashboard observability)
-- ============================================================================

-- name: InsertWebhookDelivery :exec
INSERT INTO webhook_deliveries (delivery_id, event_type, action, repo, received_at, disposition, detail)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListRecentWebhookDeliveries :many
SELECT * FROM webhook_deliveries
ORDER BY id DESC
LIMIT ?;

-- DistinctWebhookEventTypes lists the event types present in the RETAINED
-- delivery log. The mirror's correctness depends on being subscribed to a
-- specific set of events, and a missing subscription is otherwise invisible:
-- the affected caches just quietly re-fetch forever. This is what turns that
-- silence into something the dashboard can show. Bounded by the log's own
-- prune, so absence means "not in the retained window", not "never sent".
-- name: DistinctWebhookEventTypes :many
SELECT DISTINCT event_type FROM webhook_deliveries WHERE event_type <> '';

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
