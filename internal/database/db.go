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

// SchemaVersion 16: respcache round 2. commit_ci_cache gains pagination
// columns (per_page, page; the unique key is now owner/repo/ref/kind/
// per_page/page) and a third kind 'statuses_list' (the raw statuses LIST
// route); compare_cache gains base_ref/head_ref (the basehead's two sides,
// split at '...', so a push can flush per ref instead of per repo) and a
// status column (404 "unknown ref" answers become expiring miss markers);
// and three new tables -- workflow_runs_cache (per-page snapshots of
// GET /repos/{owner}/{repo}/actions/runs?head_sha=...),
// git_commit_miss_cache (expiring 404 verdicts for the single git-commit
// read; cleared by any real commit upsert so a sha that materializes stops
// answering 404), and pull_diff406_cache (406 "diff too large" verdicts for
// the single-PR diff read; 200 diff bodies are never stored). Bumping nukes
// the DB on deploy; global truth rebuilds from webhooks and each caller's
// own fetches.
// (15 added pull_files_cache, closed_pull_cache, and branches_list_cache;
// 14 was a no-schema-change nuke of truth rows poisoned by the
// collaborator-repo bleed -- repos a User login merely collaborates on,
// absorbed keyed by the WRONG owner; the fixed fetch (ownerAffiliations:
// OWNER + the dropForeignRepoNode guard in ghclient) keeps them out
// afterwards; 13 added commit_ci_cache -- trimmed combined-commit-status and
// check-runs snapshots backing the cached
// GET /repos/{owner}/{repo}/commits/{ref}/status and
// .../commits/{ref}/check-runs routes; 12 added compare_cache for the cached
// compare route; 11 added commits_list_cache for the cached commits LIST
// route; 10 was ONE GLOBAL TRUTH STORE, the global-cache re-architecture: the
// actor dimension dropped from every GitHub-state table, access decided at
// serve time by the reveal-by-permission layer; 9 was the per-actor /pulls +
// /installation cache branch, folded into that model; 8 was per-user
// partitions; 7 added workflow_jobs; 6 added the response-cache tables.)
const SchemaVersion = 16

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
