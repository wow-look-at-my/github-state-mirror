package notify

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// The subscriptions store is a SEPARATE SQLite file from the main cache DB,
// and lives by different rules — subscriptions are operator/consumer CONFIG,
// not disposable cached GitHub state:
//
//   - It is NEVER nuked. The main DB's SchemaVersion bump-to-nuke doctrine
//     applies only to the cache; this file survives every deploy.
//   - Its schema is created with CREATE TABLE IF NOT EXISTS and may only ever
//     evolve ADDITIVELY (new columns with defaults) so an old file keeps
//     working under a new binary.
//   - Queries are hand-rolled SQL in this package — a deliberate exception to
//     the sqlc doctrine: sqlc.yaml targets the main cache DB's schema, and a
//     second codegen target for one small config table is not worth the
//     entanglement.
//
// Secrets at rest: subscription secrets are stored plaintext because the
// notifier must produce an HMAC signature over every delivery body. This is
// the same trust domain as install_token_cache tokens in the main DB — the
// file sits on the same host with the same access as the traffic itself.
// Secret values are never logged and never returned by any API response.
const schemaSQL = `
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
`

// pragmas mirror the main DB's connection settings (database.Open) where they
// make sense for a small config DB.
var pragmas = []string{
	"PRAGMA journal_mode=WAL",
	"PRAGMA busy_timeout=5000",
	"PRAGMA synchronous=NORMAL",
	"PRAGMA foreign_keys=ON",
}

// Sentinel errors the API layer maps onto HTTP statuses.
var (
	// ErrNotFound: no such subscription for this principal (a foreign
	// principal's id answers the same, so existence never leaks).
	ErrNotFound = errors.New("subscription not found")
	// ErrLimitExceeded: the principal already holds MaxPerPrincipal
	// subscriptions.
	ErrLimitExceeded = fmt.Errorf("at most %d subscriptions per principal", MaxPerPrincipal)
)

// Store persists subscriptions in their own SQLite file.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the subscriptions DB at path. Unlike the cache DB
// there is no version check and no nuke path: the schema is applied
// idempotently and existing rows are always kept.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open subscriptions db: %w", err)
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("subscriptions db: exec %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("subscriptions db: apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

// newID returns "sub_" + 16 random bytes hex.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; if it somehow does,
		// an ID collision on insert would surface as a PK error.
		panic("notify: read random: " + err.Error())
	}
	return "sub_" + hex.EncodeToString(b[:])
}

// NewDeliveryID returns "ntf_" + 16 random bytes hex — unique per
// subscription x delivery.
func NewDeliveryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("notify: read random: " + err.Error())
	}
	return "ntf_" + hex.EncodeToString(b[:])
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Create validates and inserts a new subscription for principal, enforcing
// the per-principal cap inside one transaction. Validation failures return a
// *ValidationError; the cap returns ErrLimitExceeded.
func (s *Store) Create(ctx context.Context, principal string, in NewSubscription, now time.Time) (Subscription, error) {
	sub := Subscription{
		ID:           newID(),
		Principal:    principal,
		URL:          in.URL,
		Secret:       in.Secret,
		RepoFilters:  append([]string(nil), in.Repos...),
		EventFilters: append([]string(nil), in.Events...),
		Active:       true,
		CreatedAt:    rfc3339(now),
		UpdatedAt:    rfc3339(now),
	}
	if err := sub.NormalizeAndValidate(); err != nil {
		return Subscription{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Subscription{}, err
	}
	defer tx.Rollback()

	var count int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriptions WHERE principal = ?`, principal).Scan(&count); err != nil {
		return Subscription{}, err
	}
	if count >= MaxPerPrincipal {
		return Subscription{}, ErrLimitExceeded
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			id, principal, url, secret, repo_filters, event_filters, active,
			consecutive_failures, disabled_reason, created_at, updated_at,
			last_success_at, last_failure_at, last_error
		) VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, '', '', '')`,
		sub.ID, sub.Principal, sub.URL, sub.Secret,
		marshalFilters(sub.RepoFilters), marshalFilters(sub.EventFilters),
		boolToInt(sub.Active), sub.CreatedAt, sub.UpdatedAt,
	); err != nil {
		return Subscription{}, err
	}
	if err := tx.Commit(); err != nil {
		return Subscription{}, err
	}
	return sub, nil
}

// Get returns the principal's subscription with the given id, or ErrNotFound
// (including when the id belongs to another principal — no existence leak).
func (s *Store) Get(ctx context.Context, principal, id string) (Subscription, error) {
	return scanOne(s.db.QueryRowContext(ctx,
		selectCols+` FROM subscriptions WHERE id = ? AND principal = ?`, id, principal))
}

// List returns all of the principal's subscriptions, oldest first.
func (s *Store) List(ctx context.Context, principal string) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		selectCols+` FROM subscriptions WHERE principal = ? ORDER BY created_at, id`, principal)
	if err != nil {
		return nil, err
	}
	return scanAll(rows)
}

// ListAll returns every subscription (operator view), oldest first.
func (s *Store) ListAll(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, selectCols+` FROM subscriptions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return scanAll(rows)
}

// ListEnabled returns every active subscription — the notifier's fan-out set.
func (s *Store) ListEnabled(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		selectCols+` FROM subscriptions WHERE active = 1 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return scanAll(rows)
}

// Update applies a partial patch to the principal's subscription, validating
// the result. Setting Active=true resets consecutive_failures and clears
// disabled_reason (the re-enable path after an auto-disable).
func (s *Store) Update(ctx context.Context, principal, id string, p Patch, now time.Time) (Subscription, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Subscription{}, err
	}
	defer tx.Rollback()

	sub, err := scanOne(tx.QueryRowContext(ctx,
		selectCols+` FROM subscriptions WHERE id = ? AND principal = ?`, id, principal))
	if err != nil {
		return Subscription{}, err
	}

	if p.URL != nil {
		sub.URL = *p.URL
	}
	if p.Secret != nil {
		sub.Secret = *p.Secret
	}
	if p.Repos != nil {
		sub.RepoFilters = append([]string(nil), (*p.Repos)...)
	}
	if p.Events != nil {
		sub.EventFilters = append([]string(nil), (*p.Events)...)
	}
	if p.Active != nil {
		sub.Active = *p.Active
		if *p.Active {
			sub.ConsecutiveFailures = 0
			sub.DisabledReason = ""
		}
	}
	if err := sub.NormalizeAndValidate(); err != nil {
		return Subscription{}, err
	}
	sub.UpdatedAt = rfc3339(now)

	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET
			url = ?, secret = ?, repo_filters = ?, event_filters = ?, active = ?,
			consecutive_failures = ?, disabled_reason = ?, updated_at = ?
		WHERE id = ? AND principal = ?`,
		sub.URL, sub.Secret, marshalFilters(sub.RepoFilters), marshalFilters(sub.EventFilters),
		boolToInt(sub.Active), sub.ConsecutiveFailures, sub.DisabledReason, sub.UpdatedAt,
		id, principal,
	); err != nil {
		return Subscription{}, err
	}
	if err := tx.Commit(); err != nil {
		return Subscription{}, err
	}
	return sub, nil
}

// Delete removes the principal's subscription, reporting ErrNotFound for a
// missing or foreign id.
func (s *Store) Delete(ctx context.Context, principal, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM subscriptions WHERE id = ? AND principal = ?`, id, principal)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordSuccess resets the failure counter and stamps last_success_at after a
// delivered notification.
func (s *Store) RecordSuccess(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE subscriptions SET consecutive_failures = 0, last_success_at = ?, updated_at = ?
		WHERE id = ?`, rfc3339(now), rfc3339(now), id)
	return err
}

// RecordFailure increments the failure counter and stamps last_failure_at and
// last_error after a terminal delivery failure. When the counter reaches
// disableAfter the subscription is auto-disabled (active=0, disabled_reason
// set); disabled reports whether THIS call flipped it.
func (s *Store) RecordFailure(ctx context.Context, id, errMsg string, disableAfter int, now time.Time) (failures int64, disabled bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var active int64
	if err := tx.QueryRowContext(ctx,
		`SELECT consecutive_failures, active FROM subscriptions WHERE id = ?`, id).
		Scan(&failures, &active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, ErrNotFound
		}
		return 0, false, err
	}

	failures++
	newActive := active
	reason := ""
	if failures >= int64(disableAfter) && active == 1 {
		newActive = 0
		disabled = true
		reason = fmt.Sprintf("auto-disabled after %d consecutive delivery failures", failures)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET
			consecutive_failures = ?, last_failure_at = ?, last_error = ?, updated_at = ?,
			active = ?,
			disabled_reason = CASE WHEN ? != '' THEN ? ELSE disabled_reason END
		WHERE id = ?`,
		failures, rfc3339(now), errMsg, rfc3339(now), newActive, reason, reason, id,
	); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return failures, disabled, nil
}

// ---- row plumbing ----

const selectCols = `SELECT id, principal, url, secret, repo_filters, event_filters, active,
	consecutive_failures, disabled_reason, created_at, updated_at,
	last_success_at, last_failure_at, last_error`

type rowScanner interface{ Scan(dest ...any) error }

func scanOne(row rowScanner) (Subscription, error) {
	var sub Subscription
	var repoJSON, eventJSON string
	var active int64
	err := row.Scan(&sub.ID, &sub.Principal, &sub.URL, &sub.Secret, &repoJSON, &eventJSON,
		&active, &sub.ConsecutiveFailures, &sub.DisabledReason, &sub.CreatedAt, &sub.UpdatedAt,
		&sub.LastSuccessAt, &sub.LastFailureAt, &sub.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, err
	}
	sub.Active = active == 1
	sub.RepoFilters = unmarshalFilters(repoJSON)
	sub.EventFilters = unmarshalFilters(eventJSON)
	return sub, nil
}

func scanAll(rows *sql.Rows) ([]Subscription, error) {
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		sub, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func marshalFilters(fs []string) string {
	if len(fs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(fs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalFilters(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
