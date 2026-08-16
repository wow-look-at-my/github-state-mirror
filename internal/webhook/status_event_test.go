package webhook

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStatusEvent(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"sha": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "context": "all-builds",
		"state": "success", "description": "3/3 builds passed",
		"target_url": "https://rbm.example.com/b/1",
		"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:00:00Z",
	})
	require.NoError(t, err)

	got, ok := ParseStatusEvent(raw)
	require.True(t, ok)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", got.SHA, "the sha is folded like every cache key")
	assert.Equal(t, "all-builds", got.Context, "GitHub's own spelling, not the truth layer's status: prefix")
	assert.Equal(t, "success", got.State)
	require.NotNil(t, got.Description)
	assert.Equal(t, "3/3 builds passed", *got.Description)
	require.NotNil(t, got.TargetURL)
	assert.Equal(t, "2026-07-01T10:00:00Z", got.CreatedAt)
}

// A null description and target_url are what GitHub sends for a status without
// them, and the stored documents emit both keys as null -- so they must arrive
// as absent pointers rather than empty strings.
func TestParseStatusEvent_NullsStayNull(t *testing.T) {
	got, ok := ParseStatusEvent(json.RawMessage(`{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"context":"lint","state":"pending","description":null,"target_url":null,
		"created_at":"2026-07-01T10:00:00Z","updated_at":"2026-07-01T10:00:00Z"}`))
	require.True(t, ok)
	assert.Nil(t, got.Description)
	assert.Nil(t, got.TargetURL)
}

// A payload missing any field the stored documents need reports false, and the
// caller keeps the flush: a rewrite from an incomplete account of the status
// would invent the parts it did not get.
func TestParseStatusEvent_IncompletePayloadsRefuse(t *testing.T) {
	for name, body := range map[string]string{
		"no sha":        `{"context":"a","state":"success","created_at":"2026-07-01T10:00:00Z","updated_at":"2026-07-01T10:00:00Z"}`,
		"no context":    `{"sha":"a","state":"success","created_at":"2026-07-01T10:00:00Z","updated_at":"2026-07-01T10:00:00Z"}`,
		"no state":      `{"sha":"a","context":"a","created_at":"2026-07-01T10:00:00Z","updated_at":"2026-07-01T10:00:00Z"}`,
		"no created_at": `{"sha":"a","context":"a","state":"success","updated_at":"2026-07-01T10:00:00Z"}`,
		"unparseable":   `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := ParseStatusEvent(json.RawMessage(body))
			assert.False(t, ok)
		})
	}
}
