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

-- ============================================================================
-- Webhook replay requests (delivery-gap recovery)
-- ============================================================================

-- WebhookReplayRequested reports whether this delivery id has already been
-- asked for. GitHub's failure log keeps a failed delivery listed forever, so
-- without this the replayer would re-request the same one every cycle.
-- name: WebhookReplayRequested :one
SELECT EXISTS(SELECT 1 FROM webhook_replays WHERE delivery_id = ?);

-- name: RecordWebhookReplay :exec
INSERT INTO webhook_replays (delivery_id, guid, event_type, delivered_at, requested_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (delivery_id) DO NOTHING;

-- PruneWebhookReplays drops rows older than the replayer's lookback window --
-- past it a delivery is never re-requested again, so remembering it costs
-- rows for nothing.
-- name: PruneWebhookReplays :exec
DELETE FROM webhook_replays WHERE requested_at < ?;

-- ============================================================================
-- Event-order watermarks (out-of-order delivery handling)
-- ============================================================================

-- name: GetWebhookWatermark :one
SELECT * FROM webhook_watermarks WHERE subject = ?;

-- ClaimWebhookWatermark advances a subject's watermark, and ONLY forward: the
-- WHERE on the upsert is what makes the decision atomic under concurrent
-- deliveries for one subject, so the winner is decided by the payload clock
-- rather than by which request reached the database first. RETURNING yields a
-- row exactly when this view won; no rows means an equal-or-newer view is
-- already recorded.
-- name: ClaimWebhookWatermark :one
INSERT INTO webhook_watermarks (subject, event_time, delivery_id, event_type, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (subject) DO UPDATE SET
    event_time = excluded.event_time,
    delivery_id = excluded.delivery_id,
    event_type = excluded.event_type,
    updated_at = excluded.updated_at
WHERE excluded.event_time > webhook_watermarks.event_time
RETURNING event_time;

-- PruneWebhookWatermarks drops subjects nothing has touched in a while. A
-- delivery older than that window is past every TTL it could have moved, and
-- the periodic refresh has been through since.
-- name: PruneWebhookWatermarks :exec
DELETE FROM webhook_watermarks WHERE updated_at < ?;
