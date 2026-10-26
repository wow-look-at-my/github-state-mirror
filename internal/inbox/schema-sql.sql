
CREATE TABLE IF NOT EXISTS deliveries (
	delivery_id TEXT PRIMARY KEY,
	event_type  TEXT NOT NULL,
	body        BLOB NOT NULL,
	received_at INTEGER NOT NULL,
	applied_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_deliveries_pending ON deliveries (applied_at, received_at);
