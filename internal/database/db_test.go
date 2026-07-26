package database

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestOpen_CreatesNewDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := Open(path)
	require.Nil(t, err)

	defer db.Close()

	// Verify schema version was set.
	var version int64
	require.NoError(t, db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version))

	assert.Equal(t, int64(SchemaVersion), version)

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

func TestOpen_NukesOnVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	require.Nil(t, err)

	_, err = db1.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.Nil(t, err)

	// Tamper with schema version.
	_, err = db1.Exec("UPDATE schema_version SET version = 9999")
	require.Nil(t, err)

	db1.Close()

	db2, err := Open(path)
	require.Nil(t, err)

	defer db2.Close()

	// Data should be gone — DB was nuked and recreated.
	var count int
	require.NoError(t, db2.QueryRow("SELECT COUNT(*) FROM repos").Scan(&count))

	assert.Equal(t, 0, count)

	// Verify new schema version is correct.
	var version int64
	require.NoError(t, db2.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version))

	assert.Equal(t, int64(SchemaVersion), version)

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

	var version int64
	require.NoError(t, db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version))

	assert.Equal(t, int64(SchemaVersion), version)

}
