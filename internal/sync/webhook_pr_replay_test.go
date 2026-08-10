package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The incident, end to end. A PR's synchronize delivery failed; the PR then
// merged and its closed delivery applied; the replayer asked GitHub to re-send
// the failed one, and GitHub re-sent the payload it built at the time -- a
// pre-merge view. Applying it put a merged PR back in the cache as open, where
// it sat for 44 minutes with nothing to restate the close.
//
// A redelivery is indistinguishable from an ordinary late delivery, which is
// why the fix is at the write and not in the replayer.

// prPayloadAt is makePRPayload with the delivery's own updated_at, the field
// that says WHEN the view it carries was taken.
func prPayloadAt(t *testing.T, action, state, owner, repo string, number int, title, updatedAt string) json.RawMessage {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(makePRPayload(t, action, state, owner, repo, number, title), &m))
	m["pull_request"].(map[string]any)["updated_at"] = updatedAt
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return out
}

func TestDispatch_ReplayedPreCloseDeliveryDoesNotReopenAMergedPR(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "synchronize", "open", "my-org", "my-repo", 24, "PR", "2026-08-10T15:06:10Z")))
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "closed", "closed", "my-org", "my-repo", 24, "PR", "2026-08-10T15:08:18Z")))

	_, err := store.GetPullRequest(ctx, "my-org", "my-repo", 24)
	require.Equal(t, sql.ErrNoRows, err, "the close removed the row")

	// GitHub re-sends the failed synchronize. Same payload, same updated_at.
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "synchronize", "open", "my-org", "my-repo", 24, "PR", "2026-08-10T15:06:10Z")))
	assert.Equal(t, webhook.DispApplied, res.Disposition,
		"the delivery is still handled; what it may not do is write pre-close state")

	_, err = store.GetPullRequest(ctx, "my-org", "my-repo", 24)
	assert.Equal(t, sql.ErrNoRows, err, "a merged PR must not come back open")
}

func TestDispatch_ReopenedAfterACloseApplies(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "closed", "closed", "my-org", "my-repo", 24, "PR", "2026-08-10T15:08:18Z")))
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "reopened", "open", "my-org", "my-repo", 24, "PR", "2026-08-10T15:20:00Z")))

	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 24)
	require.NoError(t, err)
	assert.Equal(t, "OPEN", pr.State)
}
