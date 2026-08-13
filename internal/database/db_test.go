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

// The file every deployed instance is currently carrying: a schema_version
// table from before fingerprinting, which cannot answer the question at all.
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
