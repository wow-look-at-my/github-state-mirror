package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpen_CreatesNewDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Verify schema version was set.
	var version int64
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}

	// Verify tables exist by inserting into them.
	_, err = db.Exec("INSERT INTO users (login, avatar_url, url) VALUES ('test', 'http://avatar', 'http://url')")
	if err != nil {
		t.Fatalf("insert into users: %v", err)
	}
	_, err = db.Exec("INSERT INTO cache_metadata (resource_kind, resource_key, fetch_state) VALUES ('test', 'key', 'unknown')")
	if err != nil {
		t.Fatalf("insert into cache_metadata: %v", err)
	}
}

func TestOpen_ReopensExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	_, err = db1.Exec("INSERT INTO users (login, avatar_url, url) VALUES ('test', 'http://avatar', 'http://url')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer db2.Close()

	var login string
	if err := db2.QueryRow("SELECT login FROM users LIMIT 1").Scan(&login); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if login != "test" {
		t.Errorf("login = %q, want %q", login, "test")
	}
}

func TestOpen_NukesOnVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	_, err = db1.Exec("INSERT INTO users (login, avatar_url, url) VALUES ('test', 'http://avatar', 'http://url')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Tamper with schema version.
	_, err = db1.Exec("UPDATE schema_version SET version = 9999")
	if err != nil {
		t.Fatalf("update schema_version: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer db2.Close()

	// Data should be gone — DB was nuked and recreated.
	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Errorf("users count = %d, want 0 (DB should have been recreated)", count)
	}

	// Verify new schema version is correct.
	var version int64
	if err := db2.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}
}

func TestOpen_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "test.db")

	// Parent dir doesn't exist — should fail.
	_, err := Open(path)
	if err == nil {
		t.Fatal("expected error for missing parent dir")
	}
}

func TestOpen_FileExistsButCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Write garbage to the file.
	if err := os.WriteFile(path, []byte("not a database"), 0644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	// Should nuke and recreate.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var version int64
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}
}
