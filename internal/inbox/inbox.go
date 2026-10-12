// Package inbox is the webhook journal: every verified delivery is written
// here, synced to disk, before GitHub is answered, and stays until applied.
// It is its own SQLite file, outside the cache DB's rebuild-on-schema-change
// lifecycle. A restart never loses a delivery GitHub was told was kept.
package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS deliveries (
	delivery_id TEXT PRIMARY KEY,
	event_type  TEXT NOT NULL,
	body        BLOB NOT NULL,
	received_at INTEGER NOT NULL,
	applied_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_deliveries_pending ON deliveries (applied_at, received_at);
`

// Retention is how long an applied delivery's id is kept to recognize a redelivery.
const Retention = 72 * time.Hour

// pruneEvery throttles the deletion of applied deliveries past Retention.
const pruneEvery = 10 * time.Minute

// Store is the SQLite-backed webhook.Journal.
type Store struct {
	db        *sql.DB
	lastPrune atomic.Int64
}

var _ webhook.Journal = (*Store)(nil)

// DeriveDBPath names the journal file next to the cache DB at dbPath.
func DeriveDBPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, ".db") + "-inbox.db"
}

// Open opens (or creates) the journal at path. The schema is applied
// idempotently and rows are always kept.
func Open(path string) (*Store, error) {
	db := database.OpenDSN(database.JournalDSN(path), nil)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("webhook inbox: apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the journal.
func (s *Store) Close() error { return s.db.Close() }

// Put records d unless its delivery id is already recorded.
func (s *Store) Put(ctx context.Context, d webhook.Stored) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO deliveries (delivery_id, event_type, body, received_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (delivery_id) DO NOTHING`,
		d.DeliveryID, d.EventType, d.Body, d.ReceivedAt.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Pending returns up to limit unapplied deliveries, oldest first.
func (s *Store) Pending(ctx context.Context, limit int) ([]webhook.Stored, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT delivery_id, event_type, body, received_at FROM deliveries
		WHERE applied_at = 0 ORDER BY received_at, delivery_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webhook.Stored
	for rows.Next() {
		var d webhook.Stored
		var received int64
		if err := rows.Scan(&d.DeliveryID, &d.EventType, &d.Body, &received); err != nil {
			return nil, err
		}
		d.ReceivedAt = time.Unix(0, received)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkApplied records the delivery as applied, and now and then drops applied
// deliveries older than Retention.
func (s *Store) MarkApplied(ctx context.Context, deliveryID string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE deliveries SET applied_at = ? WHERE delivery_id = ?`, at.UnixNano(), deliveryID); err != nil {
		return err
	}
	s.maybePrune(ctx, at)
	return nil
}

func (s *Store) maybePrune(ctx context.Context, now time.Time) {
	last := s.lastPrune.Load()
	if now.UnixNano()-last < int64(pruneEvery) || !s.lastPrune.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM deliveries WHERE applied_at != 0 AND applied_at < ?`, now.Add(-Retention).UnixNano()); err != nil {
		slog.Warn("webhook inbox: pruning applied deliveries failed", "error", err)
	}
}
