package database

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScrubSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{"line comment", "CREATE TABLE t (a TEXT); -- a note\n", "CREATE TABLE t(a TEXT);"},
		{"block comment", "CREATE /* aside */ TABLE t (a TEXT);", "CREATE TABLE t(a TEXT);"},
		{"unterminated block comment", "CREATE TABLE t (a TEXT); /* dangling", "CREATE TABLE t(a TEXT);"},
		{"whitespace collapses", "CREATE\n\tTABLE   t (a\tTEXT);", "CREATE TABLE t(a TEXT);"},
		{"reflowed around punctuation", "CREATE TABLE t(\n\ta TEXT ,\n\tb TEXT\n) ;", "CREATE TABLE t(a TEXT,b TEXT);"},
		{"a comment cannot fuse its neighbours", "a--note\nb", "a b"},
		{"-- inside a string literal is data", "DEFAULT 'a -- b'", "DEFAULT 'a -- b'"},
		{"/* inside a string literal is data", "DEFAULT 'a /* b'", "DEFAULT 'a /* b'"},
		{"spacing inside a string literal is data", "DEFAULT 'a  ,  b'", "DEFAULT 'a  ,  b'"},
		{"-- inside a quoted identifier is data", `CREATE TABLE "od--d" (a TEXT);`, `CREATE TABLE "od--d"(a TEXT);`},
		{"-- inside a bracket identifier is data", "CREATE TABLE [od--d] (a TEXT);", "CREATE TABLE [od--d](a TEXT);"},
		{"a doubled quote does not end the literal", "DEFAULT 'it''s -- fine'", "DEFAULT 'it''s -- fine'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, scrubSQL(tc.sql))
		})
	}
}

// The two halves of the promise, on toy input: prose is free, structure is not.
func TestSchemaFingerprint_TracksStructureNotProse(t *testing.T) {
	base := "CREATE TABLE t (a TEXT); -- holds a thing\n"

	assert.Equal(t, schemaFingerprint(base), schemaFingerprint("CREATE TABLE t (a TEXT); -- holds a DIFFERENT thing\n"),
		"rewording a comment must not nuke a fleet's cache")
	assert.Equal(t, schemaFingerprint(base), schemaFingerprint("CREATE TABLE t (\n\ta TEXT\n); -- holds a thing\n"),
		"reindenting must not nuke either")
	assert.NotEqual(t, schemaFingerprint(base), schemaFingerprint("CREATE TABLE t (a TEXT, b TEXT); -- holds a thing\n"),
		"a new column MUST nuke -- deploying one against the old table is the breakage this prevents")
	assert.NotEqual(t, schemaFingerprint("DEFAULT 'a b'"), schemaFingerprint("DEFAULT 'a  b'"),
		"inside a literal the bytes are the value, so normalizing there would hide a real change")
}

// And on the real file, which is the input that actually ships.
func TestSchemaFingerprint_OnTheRealSchema(t *testing.T) {
	assert.Equal(t, schemaFingerprint(schemaSQL), schemaFingerprint(schemaSQL+"\n-- a note someone added later\n"))
	assert.Equal(t, schemaFingerprint(schemaSQL), schemaFingerprint(strings.ReplaceAll(schemaSQL, "\n", "\n\t")))
	assert.NotEqual(t, schemaFingerprint(schemaSQL), schemaFingerprint(schemaSQL+"\nCREATE TABLE late_arrival (a TEXT);\n"))

	assert.NotContains(t, scrubSQL(schemaSQL), "--", "every comment marker should be gone from the scrubbed schema")
}

// The promise as a deployed binary delivers it: a build whose schema.sql
// differs only in its comments reopens the file it finds, data intact.
func TestOpen_CommentOnlySchemaChangeKeepsTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	db1, err := Open(path)
	require.NoError(t, err)
	_, err = db1.Exec("INSERT INTO repos (owner, name, name_with_owner, url) VALUES ('org', 'r', 'org/r', 'http://url')")
	require.NoError(t, err)

	// Simulates the previous binary's fingerprint after a comment-only schema.sql edit.
	_, err = db1.Exec("UPDATE schema_version SET fingerprint = ?", schemaFingerprint("-- reworded\n"+schemaSQL))
	require.NoError(t, err)
	require.NoError(t, db1.Close())

	db2, err := Open(path)
	require.NoError(t, err)

	defer db2.Close()

	var count int
	require.NoError(t, db2.QueryRow("SELECT COUNT(*) FROM repos").Scan(&count))

	assert.Equal(t, 1, count, "the row must survive: nothing about the tables changed")
}
