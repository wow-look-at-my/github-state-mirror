package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The bulk lane applies a workflow_job recorded before a push after that
// push. The job's repository object then carries an older pushed_at, which
// must not move the repo's pushed_at back. A newer one still moves it forward.
func TestDispatch_BulkDeliveryNeverMovesPushedAtBack(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := ghdata.NewStore(db)
	d := NewWebhookDispatcher(freshness.NewManager(freshness.NewStore(db)), store)
	ctx := context.Background()

	dispatch := func(eventType, id string, body []byte) {
		t.Helper()
		event := webhook.ParseEvent(eventType, body)
		event.DeliveryID = id
		result := d.Dispatch(ctx, event)
		require.NotEqual(t, webhook.DispError, result.Disposition, result.Detail)
	}
	pushedAt := func() string {
		t.Helper()
		repo, err := store.GetRepo(ctx, "org1", "repo1")
		require.NoError(t, err)
		return repo.PushedAt.String
	}

	dispatch("push", "d-push", []byte(`{
		"ref": "refs/heads/main",
		"before": "1111111111111111111111111111111111111111",
		"after": "2222222222222222222222222222222222222222",
		"repository": {"name": "repo1", "full_name": "org1/repo1", "default_branch": "main", "pushed_at": 1767225600, "owner": {"login": "org1"}},
		"head_commit": {"id": "2222222222222222222222222222222222222222", "timestamp": "2026-01-01T00:00:00Z"}
	}`))
	require.Equal(t, "2026-01-01T00:00:00Z", pushedAt())

	dispatch("workflow_job", "d-job-old", []byte(`{
		"action": "queued",
		"workflow_job": {"id": 7, "run_id": 9, "status": "queued", "head_sha": "1111111111111111111111111111111111111111"},
		"repository": {"name": "repo1", "full_name": "org1/repo1", "pushed_at": "2025-12-31T00:00:00Z", "owner": {"login": "org1"}}
	}`))
	assert.Equal(t, "2026-01-01T00:00:00Z", pushedAt(), "an older view's pushed_at does not land")

	dispatch("workflow_job", "d-job-new", []byte(`{
		"action": "queued",
		"workflow_job": {"id": 8, "run_id": 10, "status": "queued", "head_sha": "2222222222222222222222222222222222222222"},
		"repository": {"name": "repo1", "full_name": "org1/repo1", "pushed_at": "2026-01-02T00:00:00Z", "owner": {"login": "org1"}}
	}`))
	assert.Equal(t, "2026-01-02T00:00:00Z", pushedAt(), "a newer view's pushed_at lands")
}
