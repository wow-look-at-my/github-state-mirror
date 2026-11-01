
CREATE TABLE IF NOT EXISTS subscriptions (
	id TEXT PRIMARY KEY,
	principal TEXT NOT NULL,
	url TEXT NOT NULL,
	secret TEXT NOT NULL,
	repo_filters TEXT NOT NULL DEFAULT '[]',
	event_filters TEXT NOT NULL DEFAULT '[]',
	active INTEGER NOT NULL DEFAULT 1,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	disabled_reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_success_at TEXT NOT NULL DEFAULT '',
	last_failure_at TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_subscriptions_principal ON subscriptions(principal);
