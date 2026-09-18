package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// mergeable_state is the merge facet a tip move does NOT invalidate: a
// check or status result flips unstable/blocked <-> clean while every sha
// stays put. It therefore has no other invalidator, and CI events are what
// must un-resolve it.

// TestDispatch_CIEventsUnresolveMergeableState: each CI event un-resolves the
// state for the open PRs on the sha it names, and touches nothing else --
// mergeable and the test-merge sha stay, because a check result cannot change
// whether trees conflict.
func TestDispatch_CIEventsUnresolveMergeableState(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const otherSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const mergeSHA = "cccccccccccccccccccccccccccccccccccccccc"

	for _, tc := range []struct {
		event string
		body  json.RawMessage
	}{
		{"status", mustJSON(t, map[string]any{
			"sha": sha, "state": "success", "context": "all-builds",
			"branches":   []any{map[string]any{"name": "main"}},
			"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
		})},
		{"check_run", mustJSON(t, map[string]any{
			"action": "completed",
			"check_run": map[string]any{
				"head_sha": sha, "status": "completed", "conclusion": "success", "name": "build",
				"check_suite": map[string]any{"head_branch": "feature"},
			},
			"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
		})},
		{"check_suite", mustJSON(t, map[string]any{
			"action": "completed",
			"check_suite": map[string]any{
				"head_sha": sha, "head_branch": "feature", "status": "completed", "conclusion": "success",
			},
			"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}},
		})},
	} {
		t.Run(tc.event, func(t *testing.T) {
			dispatcher, _, _, store := setupDispatcher(t)
			ctx := context.Background()

			seed := func(number int64, headSHA string) {
				t.Helper()
				require.NoError(t, store.UpsertPRWithChecks(ctx, dbgen.PullRequest{
					Owner: "org1", Repo: "repo1", Number: number,
					Title: "t", Url: "u", State: "OPEN",
					CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
					HeadRefName:    sql.NullString{String: "feature", Valid: true},
					BaseRefName:    sql.NullString{String: "main", Valid: true},
					HeadRefOid:     sql.NullString{String: headSHA, Valid: true},
					Mergeable:      sql.NullString{String: "MERGEABLE", Valid: true},
					MergeableState: sql.NullString{String: "blocked", Valid: true},
					MergeCommitSha: sql.NullString{String: mergeSHA, Valid: true},
				}, nil, time.Now()))
			}
			seed(7, sha)
			seed(8, otherSHA)

			dispatcher.Dispatch(ctx, webhook.ParseEvent(tc.event, tc.body))

			row, err := store.GetPullRequest(ctx, "org1", "repo1", 7)
			require.NoError(t, err)
			assert.False(t, row.MergeableState.Valid,
				"a %s event must un-resolve the state it may have moved; nothing else re-asks", tc.event)
			assert.Equal(t, "MERGEABLE", row.Mergeable.String,
				"a %s event must not touch mergeable -- a check cannot change whether two trees conflict", tc.event)
			assert.Equal(t, mergeSHA, row.MergeCommitSha.String,
				"a %s event invalidates no test merge", tc.event)

			other, err := store.GetPullRequest(ctx, "org1", "repo1", 8)
			require.NoError(t, err)
			assert.Equal(t, "blocked", other.MergeableState.String,
				"a %s event must leave a PR on another head sha alone", tc.event)
		})
	}
}

// TestDispatch_PullRequestSynchronize_UnresolvesMergeableState: the fork-head
// freeze applies to the state facet too. A synchronize carries RETAINED
// pre-move merge fields, so a 'clean' describing the old head must not be
// stored against the new -- the exact shape that made a stale answer
// serve frozen for mergeable.
func TestDispatch_PullRequestSynchronize_UnresolvesMergeableState(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	seeded := makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "PR")
	require.Equal(t, webhook.DispApplied,
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", seeded)).Disposition)
	row, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	require.Equal(t, "clean", row.MergeableState.String, "the payload's state must land on the seed")

	moved := prPayloadWithHead(t, "synchronize", 42, "def789")
	require.Equal(t, webhook.DispApplied,
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", moved)).Disposition)

	row, err = store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	assert.Equal(t, "def789", row.HeadRefOid.String)
	assert.False(t, row.MergeableState.Valid,
		"the retained pre-move state must be un-resolved with mergeable, not served against the new head")
}
