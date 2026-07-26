package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// These tests lock the push handler's merge-field invalidation against the
// two H1 holes: an error in another step of the handler silently skipping the
// invalidation, and an unparseable payload skipping it entirely. A skipped
// invalidation leaves the single-PR route serving the pre-push mergeable /
// test-merge sha as a hit -- and hits never reach GitHub, so nothing ever
// triggers the recompute (the webhooks#66 frozen-sha incident).

// seedResolvedOpenPR writes a REST-complete open PR row with a resolved
// mergeable and test-merge sha, based on the given branch.
func seedResolvedOpenPR(t *testing.T, store *ghdata.Store, number int64, base, sha string) {
	t.Helper()
	require.NoError(t, store.UpsertPR(context.Background(), dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: number,
		Title: "t", Url: "u", State: "OPEN",
		CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
		NodeID:         sql.NullString{String: "PR_node", Valid: true},
		AuthorLogin:    sql.NullString{String: "alice", Valid: true},
		HeadRefName:    sql.NullString{String: "feature", Valid: true},
		BaseRefName:    sql.NullString{String: base, Valid: true},
		HeadRefOid:     sql.NullString{String: "1111111111111111111111111111111111111111", Valid: true},
		BaseRefOid:     sql.NullString{String: "2222222222222222222222222222222222222222", Valid: true},
		Mergeable:      sql.NullString{String: "MERGEABLE", Valid: true},
		MergeCommitSha: sql.NullString{String: sha, Valid: true},
	}, time.Now()))
}

// TestOnPush_UnparseablePayloadNullsMergeFieldsRepoWide: a push whose payload
// can't be parsed still proves something moved in the repo, so ALL open PRs'
// merge fields are conservatively un-resolved (they just re-fetch) -- the old
// fallback only marked org syncs stale and left the frozen values serving.
// The moved branch is unknown, so NO stale-sha marker is recorded (an unmoved
// PR's re-offered sha is valid and must re-absorb immediately).
func TestOnPush_UnparseablePayloadNullsMergeFieldsRepoWide(t *testing.T) {
	d, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	seedResolvedOpenPR(t, store, 7, "main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	seedResolvedOpenPR(t, store, 8, "dev", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	// "ref" with the wrong type breaks ParsePushPayload; ParseEvent still
	// extracts the repo, so the fallback knows which repo moved.
	raw := []byte(`{"ref": 123, "repository": {"name": "repo1", "owner": {"login": "org1"}}}`)
	result := d.Dispatch(ctx, webhook.ParseEvent("push", raw))
	assert.Equal(t, webhook.DispInvalidated, result.Disposition)

	for _, num := range []int64{7, 8} {
		row, err := store.GetPullRequest(ctx, "org1", "repo1", num)
		require.NoError(t, err)
		assert.False(t, row.Mergeable.Valid, "PR #%d: an unparseable push must still un-resolve mergeable", num)
		assert.False(t, row.MergeCommitSha.Valid, "PR #%d: the test-merge sha must be nulled", num)
		assert.False(t, row.MergeStaleSha.Valid, "PR #%d: the repo-wide fallback must not mark a sha stale", num)
	}
}

// TestOnPush_MergeInvalidationSurvivesPushedAtFailure: the merge-field
// un-resolve must run even when a later step of the handler fails. The old
// handler ordered SetRepoPushedAt FIRST and returned on its error, so a
// transient DB failure skipped the invalidation entirely and the pre-push
// answer kept serving as a hit. The repos table is broken here (renamed) so
// SetRepoPushedAt fails exactly like a transient DB error -- pull_requests is
// untouched and the invalidation must land regardless.
func TestOnPush_MergeInvalidationSurvivesPushedAtFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := ghdata.NewStore(db)
	d := NewWebhookDispatcher(freshness.NewManager(freshness.NewStore(db)), store)
	ctx := context.Background()

	staleSha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedResolvedOpenPR(t, store, 7, "main", staleSha)

	// Break every repos-table step (absorbRepoFromPayload, SetRepoPushedAt,
	// the default-branch status reset) while pull_requests keeps working.
	_, err = db.Exec(`ALTER TABLE repos RENAME TO repos_broken`)
	require.NoError(t, err)

	raw := []byte(`{
		"ref": "refs/heads/main",
		"before": "2222222222222222222222222222222222222222",
		"after": "3333333333333333333333333333333333333333",
		"repository": {"name": "repo1", "default_branch": "main", "owner": {"login": "org1"}}
	}`)
	result := d.Dispatch(ctx, webhook.ParseEvent("push", raw))
	assert.Equal(t, webhook.DispError, result.Disposition, "the failed pushed_at apply still reports an error")

	row, err := store.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.False(t, row.Mergeable.Valid, "the un-resolve must run despite the pushed_at failure")
	assert.False(t, row.MergeCommitSha.Valid)
	assert.Equal(t, staleSha, row.MergeStaleSha.String, "the invalidated sha must be remembered")
	assert.True(t, row.MergeStaleAt.Valid)
}
