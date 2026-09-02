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

// The incident, end to end. A PR's synchronize delivery failed; the PR then
// merged and its closed delivery applied; the replayer asked GitHub to re-send
// the failed, and GitHub re-sent the payload it built at the time -- a
// pre-merge view. Applying it put a merged PR back in the cache as open, where

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
	assert.Equal(t, webhook.DispSuperseded, res.Disposition,
		"the ordering gate recognizes the older view before any write path sees it")

	_, err = store.GetPullRequest(ctx, "my-org", "my-repo", 24)
	assert.Equal(t, sql.ErrNoRows, err, "a merged PR must not come back open")
}

// The closure record is NOT made redundant by the ordering gate, and this pins
// why: the gate only sees deliveries. A PR row written by a FETCH absorb -- the
// single-PR route, a list page, the consistency reconcile -- passes no
// watermark, so the write itself has to refuse a view that cannot prove it
// postdates a recorded close. independent guards, shared failure.
func TestDispatch_ClosureRecordStillRefusesAPreCloseViewWithoutTheGate(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "closed", "closed", "my-org", "my-repo", 31, "PR", "2026-08-10T15:08:18Z")))

	// The absorb path a fetched answer takes, with a pre-close view.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 31, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-08-10T15:00:00Z", UpdatedAt: "2026-08-10T15:06:10Z",
	}, time.Now()))

	_, err := store.GetPullRequest(ctx, "my-org", "my-repo", 31)
	assert.Equal(t, sql.ErrNoRows, err, "the write refuses a view that predates the recorded close")
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
