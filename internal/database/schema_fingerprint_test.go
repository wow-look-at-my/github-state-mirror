package database

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

// An existing DB file is never migrated, only nuked, and only when
// SchemaVersion stops matching the version recorded inside it. So a schema
// edit that leaves the number alone ships a binary that writes columns the
// deployed table does not have, while every test here passes against a DB
// built fresh from the new schema.sql -- there is no test a schema change
// alone can fail. That is why the pair is pinned here: editing schema.sql
// fails this until SchemaVersion and this fingerprint move with it.
const schemaFingerprint = "3e8c09360b8eb770dd73349a4bd4b47520d9c4b2f36aff5d6e898846a601c625"

func TestSchemaVersionTracksSchema(t *testing.T) {
	sum := sha256.Sum256([]byte(schemaSQL))
	assert.Equal(t, schemaFingerprint, hex.EncodeToString(sum[:]),
		"schema.sql changed: bump SchemaVersion in db.go and describe the new version in the comment above it, then set schemaFingerprint to the sum reported here -- all in this same commit")
}
