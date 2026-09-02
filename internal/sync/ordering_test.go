package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The out-of-order gate. GitHub orders nothing; these pin that a view older
// than already applied never writes, that the grain is fine enough for
// "older" to mean "superseded", and that what a refused delivery still carries
// -- facts that cannot be stale -- is kept.

func statusPayload(t *testing.T, repo, sha, context, state, updatedAt string) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": 1, "sha": sha, "context": context, "state": state, "updated_at": updatedAt,
		"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}, "full_name": repo},
	})
	require.NoError(t, err)
	return body
}

func pushPayloadAt(t *testing.T, ref, after string, pushedAt time.Time, commits []map[string]any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"ref": ref, "before": "1111111111111111111111111111111111111111", "after": after,
		"commits":     commits,
		"head_commit": map[string]any{"id": after, "timestamp": "2020-01-01T00:00:00Z"},
		"repository": map[string]any{
			"name": "repo1", "owner": map[string]any{"login": "org1"}, "full_name": "org1/repo1",
			"default_branch": "main", "pushed_at": pushedAt.Unix(),
		},
	})
	require.NoError(t, err)
	return body
}

func TestOrdering_OlderViewOfOneSubjectNeverWrites(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	newer := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 7, "Current title", "2026-08-10T15:08:18Z")))
	require.Equal(t, webhook.DispApplied, newer.Disposition)

	older := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 7, "Stale title", "2026-08-10T15:06:10Z")))
	assert.Equal(t, webhook.DispSuperseded, older.Disposition)
	assert.Contains(t, older.Detail, "pr:org1/repo1#7")

	pr, err := store.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.Equal(t, "Current title", pr.Title, "the newer view must survive the older one arriving after it")

	stats := dispatcher.Ordering()
	assert.Equal(t, int64(1), stats.Superseded)
	assert.Equal(t, int64(1), stats.SupersededBeyond, "128s behind is past the jitter band")
	assert.Equal(t, int64(1), stats.ByEvent["pull_request"])
}

// Equal timestamps apply. GitHub's payload clocks are -granular, so
// distinct views of subject can share a timestamp and refusing on equality
// would drop the on rounding alone.
func TestOrdering_EqualTimestampsApply(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 8, "First", "2026-08-10T15:08:18Z")))
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 8, "Second", "2026-08-10T15:08:18Z")))
	assert.Equal(t, webhook.DispApplied, res.Disposition)

	pr, err := store.GetPullRequest(ctx, "org1", "repo1", 8)
	require.NoError(t, err)
	assert.Equal(t, "Second", pr.Title)
	assert.Equal(t, int64(0), dispatcher.Ordering().Superseded)
}

// Different subjects are independent: PR's newer event must never make
// another PR's older-but--seen event look late.
func TestOrdering_SubjectsAreIndependent(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 1, "PR one", "2026-08-10T15:08:18Z")))
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 2, "PR two", "2026-08-10T15:00:00Z")))
	assert.Equal(t, webhook.DispApplied, res.Disposition)

	pr, err := store.GetPullRequest(ctx, "org1", "repo1", 2)
	require.NoError(t, err)
	assert.Equal(t, "PR two", pr.Title)
}

// The grain that matters most: commit carries many status CONTEXTS, and
// they supersede only themselves. Keyed on the sha alone, `all-builds` landing
//
//	would discard a `ci` result posted a earlier -- silently losing
//
// a check the mirror is the source of truth for.
func TestOrdering_StatusContextsOnOneShaDoNotSupersedeEachOther(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	dispatcher.Dispatch(ctx, webhook.ParseEvent("status",
		statusPayload(t, "org1/repo1", sha, "all-builds", "success", "2026-08-10T15:08:18Z")))
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("status",
		statusPayload(t, "org1/repo1", sha, "ci", "failure", "2026-08-10T15:06:10Z")))
	assert.Equal(t, webhook.DispApplied, res.Disposition, "a different context is a different subject")

	states, err := store.CommitCheckStates(ctx, "org1", "repo1", sha)
	require.NoError(t, err)
	assert.Len(t, states, 2, "both contexts must be stored")

	// The SAME context, older, is refused.
	again := dispatcher.Dispatch(ctx, webhook.ParseEvent("status",
		statusPayload(t, "org1/repo1", sha, "all-builds", "failure", "2026-08-10T15:00:00Z")))
	assert.Equal(t, webhook.DispSuperseded, again.Disposition)
}

// The thing a reordering buffer would have bought: a push carries commit
// objects the newer push never restates. They are immutable and
// content-addressed, so absorbing them out of order is not a stale write --
// dropping them would just mean fetching them back later.
func TestOrdering_SupersededPushStillAbsorbsItsCommits(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const oldSHA = "cccccccccccccccccccccccccccccccccccccccc"

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", pushPayloadAt(t, "refs/heads/main",
		"dddddddddddddddddddddddddddddddddddddddd", now, nil)))

	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("push", pushPayloadAt(t, "refs/heads/main",
		oldSHA, now.Add(-30*time.Second), []map[string]any{{
			"id": oldSHA, "tree_id": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			"message": "an earlier push", "timestamp": "2026-08-10T15:00:00Z",
			"author":    map[string]any{"name": "a", "email": "a@example.com"},
			"committer": map[string]any{"name": "a", "email": "a@example.com"},
		}})))
	assert.Equal(t, webhook.DispSuperseded, res.Disposition)
	assert.Contains(t, res.Detail, "immutable commits were still absorbed")

	_, ok, err := store.GetCachedGitCommit(ctx, "org1", "repo1", oldSHA, time.Now())
	require.NoError(t, err)
	assert.True(t, ok, "an out-of-order push must still contribute its commits")

	sample := dispatcher.Ordering().Recent
	require.Len(t, sample, 1)
	assert.True(t, sample[0].StillAbsorbed)
	assert.Equal(t, "ref:org1/repo1:refs/heads/main", sample[0].Subject)
	assert.InDelta(t, 30, sample[0].LatenessSeconds, 1.5)
	assert.Equal(t, "repository.pushed_at", sample[0].ClockField)
}

// A payload with no clock this service can use applies as it always did, and
// is counted as unorderable rather than silently treated as in-order.
func TestOrdering_UnorderablePayloadStillApplies(t *testing.T) {
	dispatcher, _, _, _ := setupDispatcher(t)
	ctx := context.Background()

	body, err := json.Marshal(map[string]any{
		"action": "created", "label": map[string]any{"name": "bug", "color": "ff0000"},
		"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}, "full_name": "org1/repo1"},
	})
	require.NoError(t, err)
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("label", body))
	assert.NotEqual(t, webhook.DispSuperseded, res.Disposition)

	stats := dispatcher.Ordering()
	assert.Equal(t, int64(1), stats.Unorderable)
	assert.Equal(t, int64(0), stats.Superseded)
}

// The lateness bands mean different things operationally: jitter versus a
// delivery something held. Both refuse; the split is what the dashboard reads.
func TestOrdering_LatenessBandsAndSampleBound(t *testing.T) {
	dispatcher, _, _, _ := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 9, "now", "2026-08-10T15:10:00Z")))
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 9, "jitter", "2026-08-10T15:09:56Z")))
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 9, "held", "2026-08-10T15:05:00Z")))

	stats := dispatcher.Ordering()
	assert.Equal(t, int64(2), stats.Superseded)
	assert.Equal(t, int64(1), stats.SupersededWithin, "4s behind is ordinary jitter")
	assert.Equal(t, int64(1), stats.SupersededBeyond, "5 minutes behind is not")
	assert.InDelta(t, 300, stats.WorstLatenessSeconds, 0.5)
	assert.Equal(t, float64(10), stats.GraceSeconds)

	for i := 0; i < OutOfOrderSampleLimit+5; i++ {
		dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
			prPayloadAt(t, "edited", "open", "org1", "repo1", 9, fmt.Sprintf("late %d", i), "2026-08-10T15:00:00Z")))
	}
	assert.Len(t, dispatcher.Ordering().Recent, OutOfOrderSampleLimit, "the sample ring stays bounded")
}

// A watermark must never cost a delivery: if the store cannot answer, the
// event applies exactly as it did before the gate existed.
func TestOrdering_StoreFailureAppliesTheDelivery(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	dispatcher := NewWebhookDispatcher(freshness.NewManager(freshness.NewStore(db)), ghdata.NewStore(db))
	require.NoError(t, db.Close()) // every watermark read now errors

	res := dispatcher.Dispatch(context.Background(), webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 10, "PR", "2026-08-10T15:08:18Z")))
	assert.NotEqual(t, webhook.DispSuperseded, res.Disposition, "a broken gate must not swallow deliveries")
	assert.Equal(t, int64(1), dispatcher.Ordering().Failed)
}
