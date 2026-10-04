package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingObserver struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *countingObserver) Observe(kind string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n[kind]++
}

func (o *countingObserver) count(kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n[kind]
}

func fingerprints(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT fingerprint FROM schema_version WHERE fingerprint LIKE 'tx-%' ORDER BY fingerprint")
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		require.NoError(t, rows.Scan(&f))
		out = append(out, f)
	}
	require.NoError(t, rows.Err())
	return out
}

// Every write made with InTx's context lands in one transaction: the write lock is taken once.
func TestInTx_TakesTheWriteLockOnce(t *testing.T) {
	obs := &countingObserver{n: map[string]int{}}
	db, err := OpenObserved(filepath.Join(t.TempDir(), "test.db"), obs)
	require.NoError(t, err)
	defer db.Close()
	before := obs.count(KindWriteWait)

	require.NoError(t, InTx(context.Background(), db, func(ctx context.Context) error {
		for _, f := range []string{"tx-a", "tx-b", "tx-c"} {
			tx, err := Begin(ctx, db)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO schema_version (fingerprint) VALUES (?)", f); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		return nil
	}))

	assert.Equal(t, 1, obs.count(KindWriteWait)-before, "three scoped writes, one admission to the write lock")
	assert.Equal(t, []string{"tx-a", "tx-b", "tx-c"}, fingerprints(t, db))
}

// A scope opened inside InTx is a savepoint: rolling it back undoes only its own writes.
func TestInTx_NestedRollbackUndoesOnlyItsScope(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, InTx(context.Background(), db, func(ctx context.Context) error {
		kept, err := Begin(ctx, db)
		require.NoError(t, err)
		_, err = kept.ExecContext(ctx, "INSERT INTO schema_version (fingerprint) VALUES ('tx-kept')")
		require.NoError(t, err)
		require.NoError(t, kept.Commit())

		dropped, err := Begin(ctx, db)
		require.NoError(t, err)
		_, err = dropped.ExecContext(ctx, "INSERT INTO schema_version (fingerprint) VALUES ('tx-dropped')")
		require.NoError(t, err)
		require.NoError(t, dropped.Rollback())
		return nil
	}))

	assert.Equal(t, []string{"tx-kept"}, fingerprints(t, db))
}

// An error from fn rolls back everything fn wrote.
func TestInTx_ErrorRollsBackTheWholeScope(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	boom := assert.AnError
	err = InTx(context.Background(), db, func(ctx context.Context) error {
		_, err := TxFrom(ctx, db).ExecContext(ctx, "INSERT INTO schema_version (fingerprint) VALUES ('tx-gone')")
		require.NoError(t, err)
		return boom
	})
	assert.ErrorIs(t, err, boom)
	assert.Empty(t, fingerprints(t, db))
}

// A write sent to the pool with the transaction's context would wait on the
// lock that same transaction holds; it fails at once instead.
func TestInTx_WriteToThePoolFailsInsteadOfWaitingOnItself(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	start := time.Now()
	require.NoError(t, InTx(context.Background(), db, func(ctx context.Context) error {
		_, err := db.ExecContext(ctx, "INSERT INTO schema_version (fingerprint) VALUES ('tx-pool')")
		assert.ErrorIs(t, err, ErrWriteOutsideTx)
		_, err = db.BeginTx(ctx, nil)
		assert.ErrorIs(t, err, ErrWriteOutsideTx)

		var n int
		assert.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_version").Scan(&n), "a read on the pool never needs the lock")
		return nil
	}))
	assert.Less(t, time.Since(start), writeGateWait/2)
	assert.Empty(t, fingerprints(t, db))
}

// A context's transaction on one database does not stop writes to another.
func TestInTx_OtherDatabaseWritesFreely(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "a.db"))
	require.NoError(t, err)
	defer a.Close()
	b, err := Open(filepath.Join(dir, "b.db"))
	require.NoError(t, err)
	defer b.Close()

	require.NoError(t, InTx(context.Background(), a, func(ctx context.Context) error {
		assert.Nil(t, TxFrom(ctx, b))
		_, err := b.ExecContext(ctx, "INSERT INTO schema_version (fingerprint) VALUES ('tx-b')")
		return err
	}))
	assert.Equal(t, []string{"tx-b"}, fingerprints(t, b))
}
