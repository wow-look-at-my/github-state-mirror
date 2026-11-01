// schemaSQL is the whole schema: created together, fingerprinted together.

package database

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var sqlcSchema string

//go:embed schema_plain.sql
var plainSchema string

// schemaSQL is the whole schema: created together, fingerprinted together.
var schemaSQL = sqlcSchema + "\n" + plainSchema

// What each schema change was and why: docs/schema-history.md.

// SQLite scopes pragmas to one connection, so the DSN applies them to every connection the pool opens.
const sharedPragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate"

// DSN is for the cache DB. It never fsyncs: the kernel flushes the file when
// it chooses. A power loss can corrupt the file, and Open then rebuilds it.
func DSN(path string) string {
	return path + "?" + sharedPragmas + "&_pragma=synchronous(OFF)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-65536)&_pragma=mmap_size(268435456)"
}

// DurableDSN is for a DB that is never rebuilt, so a commit must survive a power loss.
func DurableDSN(path string) string {
	return path + "?" + sharedPragmas + "&_pragma=synchronous(NORMAL)"
}

// JournalDSN is for a DB whose commit is a promise to someone else: the commit
// returns only once the WAL frame is on disk.
func JournalDSN(path string) string {
	return path + "?" + sharedPragmas + "&_pragma=synchronous(FULL)"
}

// Open opens (or creates) the SQLite database at path.
func Open(path string) (*sql.DB, error) { return OpenObserved(path, nil) }

// OpenObserved is Open with every SQLite call timed into obs. A nil obs times nothing.
func OpenObserved(path string, obs Observer) (*sql.DB, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return createFresh(path, obs)
	} else if err != nil {
		return nil, fmt.Errorf("stat db: %w", err)
	}

	// File exists — try to open and verify.
	db, err := openAndConfigure(path, obs)
	if err != nil {
		// Corrupt or unreadable — nuke it.
		os.Remove(path)
		return createFresh(path, obs)
	}

	var fingerprint string
	if err := db.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint); err != nil || fingerprint != currentFingerprint() {
		// Different schema, or a file too old to state which — nuke and recreate.
		db.Close()
		os.Remove(path)
		return createFresh(path, obs)
	}

	return db, nil
}

func createFresh(path string, obs Observer) (*sql.DB, error) {
	db, err := openAndConfigure(path, obs)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.Exec("INSERT INTO schema_version (fingerprint) VALUES (?)", currentFingerprint()); err != nil {
		db.Close()
		return nil, fmt.Errorf("set schema fingerprint: %w", err)
	}
	return db, nil
}

const maxIdleConns = 8

// OpenDSN opens dsn with every write admitted through one write gate (gate.go)
// and every call timed into obs (nil times nothing). It does no schema check.
func OpenDSN(dsn string, obs Observer) *sql.DB {
	gate := newWriteGate(obs)
	db := sql.OpenDB(observedConnector{dsn: dsn, drv: baseDriver(), obs: obs, gate: gate})
	db.SetMaxIdleConns(maxIdleConns)
	gates.Store(db, gate)
	return db
}

func openAndConfigure(path string, obs Observer) (*sql.DB, error) {
	db := OpenDSN(DSN(path), obs)
	// sql.OpenDB is lazy; Ping opens a connection, so a bad file fails here and gets nuked.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure db: %w", err)
	}
	return db, nil
}
