package sync

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The fork-head freeze, end to end through the dispatcher: a synchronize
// delivery carries the NEW head sha next to RETAINED pre-move merge fields
// (mergeable true + the old test-merge sha). Fork heads emit no push webhook
// to un-resolve the row first, so without the tip-move un-resolve the upsert
// preserved the stale resolved answer and served it frozen.
func TestDispatch_PullRequestSynchronize_UnresolvesMergeFields(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	// Seed: the PR as first delivered (head abc123, resolved MERGEABLE).
	seeded := makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "PR")
	require.Equal(t, webhook.DispApplied,
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", seeded)).Disposition)
	row, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	require.Equal(t, "MERGEABLE", row.Mergeable.String, "seed must be resolved")
	require.Equal(t, "abc123", row.HeadRefOid.String)

	// A fork-head synchronize: new head sha, mergeable retained true (the
	// pre-move answer GitHub redelivers before its recompute finishes).
	moved := prPayloadWithHead(t, "synchronize", 42, "def789")
	require.Equal(t, webhook.DispApplied,
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", moved)).Disposition)

	row, err = store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	assert.Equal(t, "def789", row.HeadRefOid.String, "the new head must land")
	assert.False(t, row.Mergeable.Valid,
		"the retained pre-move mergeable must be un-resolved, never served frozen against the new head")
}

// A same-head redelivery must NOT disturb the resolved answer -- the
// un-resolve triggers only on a real tip move.
func TestDispatch_PullRequestRedelivery_KeepsResolvedMergeable(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	seeded := makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "PR")
	require.Equal(t, webhook.DispApplied,
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", seeded)).Disposition)

	redelivered := makePRPayload(t, "edited", "open", "my-org", "my-repo", 42, "PR (retitled)")
	require.Equal(t, webhook.DispApplied,
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", redelivered)).Disposition)

	row, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	assert.Equal(t, "MERGEABLE", row.Mergeable.String,
		"an unchanged tip must keep the resolved answer")
}

// prPayloadWithHead is makePRPayload with the head sha overridden -- the
// synchronize shape (GitHub retains the pre-move mergeable/merge_commit_sha
// in the payload; makePRPayload's mergeable:true stands in for that).
func prPayloadWithHead(t *testing.T, action string, number int, headSHA string) []byte {
	t.Helper()
	raw := makePRPayload(t, action, "open", "my-org", "my-repo", number, "PR")
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &body))
	pr := body["pull_request"].(map[string]interface{})
	pr["head"].(map[string]interface{})["sha"] = headSHA
	out, err := json.Marshal(body)
	require.NoError(t, err)
	return out
}
