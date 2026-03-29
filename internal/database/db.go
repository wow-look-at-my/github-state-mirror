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

const SchemaVersion = 1

// Open opens (or creates) the SQLite database at path.
// If the schema version doesn't match, the file is deleted and recreated.
func Open(path string) (*sql.DB, error) {
	needsCreate := false

	if _, err := os.Stat(path); os.IsNotExist(err) {
		needsCreate = true
	} else if err != nil {
		return nil, fmt.Errorf("stat db: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// SQLite pragmas for performance.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", pragma, err)
		}
	}

	if !needsCreate {
		var version int64
		err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
		if err != nil || version != SchemaVersion {
			// Version mismatch or missing — nuke and recreate.
			db.Close()
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("remove stale db: %w", err)
			}
			needsCreate = true
			db, err = sql.Open("sqlite", path)
			if err != nil {
				return nil, fmt.Errorf("reopen db: %w", err)
			}
			for _, pragma := range []string{
				"PRAGMA journal_mode=WAL",
				"PRAGMA busy_timeout=5000",
				"PRAGMA synchronous=NORMAL",
				"PRAGMA foreign_keys=ON",
			} {
				if _, err := db.Exec(pragma); err != nil {
					db.Close()
					return nil, fmt.Errorf("exec %q: %w", pragma, err)
				}
			}
		}
	}

	if needsCreate {
		if _, err := db.Exec(schemaSQL); err != nil {
			db.Close()
			return nil, fmt.Errorf("create schema: %w", err)
		}
		if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", SchemaVersion); err != nil {
			db.Close()
			return nil, fmt.Errorf("set schema version: %w", err)
		}
	}

	return db, nil
}
