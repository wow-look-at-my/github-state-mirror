package database

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A nuke has to survive the shutdown that never got to close the DB: the
// container is killed, the -wal keeps committed frames the main file has not
// absorbed, and the next boot is the one that finds a schema it does not
// recognize. Two things are load-bearing there, and neither is visible at the
// call site. The recorded fingerprint is read AFTER WAL recovery, so the
// decision is made on the file's real latest state rather than on a stale main
// file. And the mismatch branch closes the handle before deleting the file,
// which is what clears the sidecars -- deleting the main file alone would
// leave that WAL sitting beside a brand-new database.
func TestOpen_NukesAcrossAnUncleanShutdown(t *testing.T) {
	live := filepath.Join(t.TempDir(), "test.db")

	db1, err := Open(live)
	require.NoError(t, err)

	// Both writes land in the WAL, and nothing checkpoints them.
	_, err = db1.Exec("UPDATE schema_version SET fingerprint = 'a-schema-this-binary-was-not-built-against'")
	require.NoError(t, err)
	_, err = db1.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.NoError(t, err)

	// Snapshot mid-flight: the on-disk state a killed process leaves behind.
	crashed := filepath.Join(t.TempDir(), "test.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(live + suffix)
		require.NoError(t, err, "expected %s to exist while the DB is open", live+suffix)
		require.NoError(t, os.WriteFile(crashed+suffix, b, 0o644))
	}

	wal, err := os.Stat(crashed + "-wal")
	require.NoError(t, err)
	require.NotZero(t, wal.Size(), "the snapshot is only meaningful if the WAL still holds those writes")

	db1.Close()

	db2, err := Open(crashed)
	require.NoError(t, err)

	defer db2.Close()

	var count int
	require.NoError(t, db2.QueryRow("SELECT COUNT(*) FROM repos").Scan(&count))

	assert.Equal(t, 0, count, "the WAL named another schema, so its rows must not survive into the rebuilt DB")

	var fingerprint string
	require.NoError(t, db2.QueryRow("SELECT fingerprint FROM schema_version LIMIT 1").Scan(&fingerprint))

	assert.Equal(t, currentFingerprint(), fingerprint)

	_, err = db2.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r2', 'org/r2', 'http://url')")
	assert.NoError(t, err, "and it must be a working database, not one wearing a stale WAL")
}
