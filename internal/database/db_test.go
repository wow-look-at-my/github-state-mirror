package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_CreatesNewDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := Open(path)
	require.Nil(t, err)

	defer db.Close()

	// Verify the schema fingerprint was recorded.
	var fingerprint string
	require.NoError(t, db.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint))

	assert.Equal(t, schemaFingerprint(schemaSQL), fingerprint)

	// Verify tables exist by inserting into them.
	_, err = db.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.Nil(t, err)

	_, err = db.Exec("INSERT INTO cache_metadata (resource_kind, resource_key, fetch_state) VALUES ('test', 'key', 'unknown')")
	require.Nil(t, err)

}

func TestOpen_ReopensExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	require.Nil(t, err)

	_, err = db1.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.Nil(t, err)

	db1.Close()

	db2, err := Open(path)
	require.Nil(t, err)

	defer db2.Close()

	var owner string
	require.NoError(t, db2.QueryRow("SELECT owner FROM repos LIMIT 1").Scan(&owner))

	assert.Equal(t, "org", owner)

}

func TestOpen_NukesOnSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	require.Nil(t, err)

	_, err = db1.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.Nil(t, err)

	// Stand in for a file written by a binary whose schema.sql differed.
	_, err = db1.Exec("UPDATE schema_version SET fingerprint = 'a-schema-this-binary-was-not-built-against'")
	require.Nil(t, err)

	db1.Close()

	db2, err := Open(path)
	require.Nil(t, err)

	defer db2.Close()

	// Data should be gone — DB was nuked and recreated.
	var count int
	require.NoError(t, db2.QueryRow("SELECT COUNT(*) FROM repos").Scan(&count))

	assert.Equal(t, 0, count)

	// Verify the rebuilt file records this binary's schema.
	var fingerprint string
	require.NoError(t, db2.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint))

	assert.Equal(t, schemaFingerprint(schemaSQL), fingerprint)

}

// The file every deployed instance is carrying: a schema_version table from
// before fingerprinting, which cannot answer the question at all.
func TestOpen_NukesAFileThatPredatesFingerprinting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	require.Nil(t, err)

	for _, stmt := range []string{
		"INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')",
		"DROP TABLE schema_version",
		"CREATE TABLE schema_version (version INTEGER NOT NULL)",
		"INSERT INTO schema_version (version) VALUES (26)",
	} {
		_, err = db1.Exec(stmt)
		require.NoError(t, err, stmt)
	}

	db1.Close()

	db2, err := Open(path)
	require.Nil(t, err)

	defer db2.Close()

	var count int
	require.NoError(t, db2.QueryRow("SELECT COUNT(*) FROM repos").Scan(&count))

	assert.Equal(t, 0, count, "an unreadable answer is not a matching one")

	var fingerprint string
	require.NoError(t, db2.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint))

	assert.Equal(t, schemaFingerprint(schemaSQL), fingerprint)

}

// SQLite scopes busy_timeout and foreign_keys to ONE connection. Every
// connection the pool opens must carry them, not only the first one.
func TestOpen_EveryPooledConnectionCarriesThePragmas(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	var conns []*sql.Conn
	for range 4 {
		c, err := db.Conn(ctx)
		require.NoError(t, err)
		conns = append(conns, c)
	}
	for i, c := range conns {
		var busy, fk, syncMode int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy))
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode))
		assert.Equal(t, 30000, busy, "connection %d has no busy_timeout: a concurrent writer gets SQLITE_BUSY at once", i)
		assert.Equal(t, 1, fk, "connection %d has foreign_keys off", i)
		assert.Equal(t, 0, syncMode, "connection %d fsyncs: the cache DB runs with synchronous=OFF", i)
	}
	for _, c := range conns {
		require.NoError(t, c.Close())
	}
}

// A burst of webhook deliveries writes from many goroutines at once. Each
// read-then-write transaction must wait for the lock, never fail with SQLITE_BUSY.
func TestOpen_ConcurrentReadThenWriteTransactionsDoNotFail(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := db.Begin()
			if err != nil {
				errs <- err
				return
			}
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM repos").Scan(&n); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			name := fmt.Sprintf("r%d", i)
			if _, err := tx.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', ?, ?, 'http://url')", name, "org/"+name); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM repos").Scan(&count))
	assert.Equal(t, writers, count)
}

type kindCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (k *kindCounter) Observe(kind string, _ time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.n[kind]++
}

func TestOpenObserved_TimesEveryKindOfCall(t *testing.T) {
	obs := &kindCounter{n: map[string]int{}}
	db, err := OpenObserved(filepath.Join(t.TempDir(), "test.db"), obs)
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM repos").Scan(&n))
	assert.Equal(t, 1, n)

	for _, kind := range []string{KindBegin, KindExec, KindQuery, KindCommit} {
		assert.Positive(t, obs.n[kind], "%s was never observed", kind)
	}
}

func TestOpen_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "test.db")

	// Parent dir doesn't exist — should fail.
	_, err := Open(path)
	require.NotNil(t, err)

}

func TestOpen_FileExistsButCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Write garbage to the file.
	require.NoError(t, os.WriteFile(path, []byte("not a database"), 0644))

	// Should nuke and recreate.
	db, err := Open(path)
	require.Nil(t, err)

	defer db.Close()

	var fingerprint string
	require.NoError(t, db.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint))

	assert.Equal(t, schemaFingerprint(schemaSQL), fingerprint)

}
