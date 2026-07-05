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

// SchemaVersion 11: adds commits_list_cache -- per-page snapshots backing the
// cached GET /repos/{owner}/{repo}/commits list route (the listed commits
// themselves are absorbed into git_commits_cache). Bumping nukes the DB on
// deploy; global truth rebuilds from webhooks and each caller's own fetches.
// (10 was ONE GLOBAL TRUTH STORE, the global-cache re-architecture: the actor
// dimension dropped from every GitHub-state table, access decided at serve
// time by the reveal-by-permission layer; 9 was the per-actor /pulls +
// /installation cache branch, folded into that model; 8 was per-user
// partitions; 7 added workflow_jobs; 6 added the response-cache tables.)
const SchemaVersion = 11

var pragmas = []string{
	"PRAGMA journal_mode=WAL",
	"PRAGMA busy_timeout=5000",
	"PRAGMA synchronous=NORMAL",
	"PRAGMA foreign_keys=ON",
}

// Open opens (or creates) the SQLite database at path.
// If the schema version doesn't match or the DB is corrupt, the file is
// deleted and recreated. This is a cache — data loss is acceptable.
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

	var version int64
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil || version != SchemaVersion {
		// Version mismatch or schema missing — nuke and recreate.
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
	if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", SchemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("set schema version: %w", err)
	}
	return db, nil
}

func openAndConfigure(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", pragma, err)
		}
	}
	return db, nil
}
