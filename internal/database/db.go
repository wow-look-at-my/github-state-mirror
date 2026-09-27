package database

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// What each schema change was and why: docs/schema-history.md.

// DSN is the driver connection string for the SQLite file at path. SQLite
// scopes these pragmas to one connection, so the DSN applies them to every
// connection the pool opens.
func DSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"
}

// Open opens (or creates) the SQLite database at path. A file whose schema is
// not the this binary was built against, or that cannot be read at all, is
// deleted and rebuilt. This is a cache — data loss is acceptable.
//
// A schema change is the ONLY thing that rebuilds it, and it always does: the
// comparison is against schema.sql's own scrubbed fingerprint, so there is no
// declaration to keep in step with the file and no lever to pull without.
func Open(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return createFresh(path)
	} else if err != nil {
		return nil, fmt.Errorf("stat db: %w", err)
	}

	// File exists — try to open and verify.
	db, err := openAndConfigure(path)
	if err != nil {
		// Corrupt or unreadable — nuke it.
		os.Remove(path)
		return createFresh(path)
	}

	var fingerprint string
	if err := db.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint); err != nil || fingerprint != currentFingerprint() {
		// Different schema, or a file too old to state which — nuke and recreate.
		db.Close()
		os.Remove(path)
		return createFresh(path)
	}

	return db, nil
}

func createFresh(path string) (*sql.DB, error) {
	db, err := openAndConfigure(path)
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

func openAndConfigure(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// sql.Open is lazy; Ping opens a connection, so a bad file fails here and gets nuked.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure db: %w", err)
	}
	return db, nil
}
