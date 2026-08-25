package webhook

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A webhook-maintained row has to be rebuild-complete for the cached single-PR
// route, and mergeable_state is the field that says a strict up-to-date rule
// is what blocks the merge. Dropping it here would leave the stored state
// frozen beside a mergeable the same payload just refreshed.
func TestParsePRPayload_MergeableState(t *testing.T) {
	payloadWith := func(state any) json.RawMessage {
		pr := map[string]any{
			"number": 42, "title": "t", "html_url": "u", "draft": false, "state": "open",
			"created_at": "2026-04-01T10:00:00Z", "updated_at": "2026-04-01T10:00:00Z",
			"mergeable": true,
			"head":      map[string]any{"ref": "feature", "sha": "abc123"},
			"base": map[string]any{"ref": "main", "repo": map[string]any{
				"name": "my-repo", "owner": map[string]any{"login": "my-org"},
			}},
		}
		if state != nil {
			pr["mergeable_state"] = state
		}
		data, err := json.Marshal(map[string]any{"action": "opened", "pull_request": pr})
		require.NoError(t, err)
		return data
	}

	got, err := ParsePRPayload(payloadWith("behind"))
	require.NoError(t, err)
	assert.Equal(t, "behind", got.PR.MergeableState.String, "the state must survive parsing verbatim")

	// "unknown" is GitHub still computing, not an answer: storing it would
	got, err = ParsePRPayload(payloadWith("unknown"))
	require.NoError(t, err)
	assert.False(t, got.PR.MergeableState.Valid, "'unknown' must stay unresolved")

	// An absent field leaves the column untouched at the upsert (COALESCE),
	got, err = ParsePRPayload(payloadWith(nil))
	require.NoError(t, err)
	assert.False(t, got.PR.MergeableState.Valid, "an absent field must not be stored as empty")
}
