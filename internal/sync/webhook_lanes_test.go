package sync

import _ "embed"

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

	dispatch("push", "d-push", []byte(testDispatch_BulkDeliveryNeverMovesPushedAtBackJson))
	require.Equal(t, "2026-01-01T00:00:00Z", pushedAt())

	dispatch("workflow_job", "d-job-old", []byte(testDispatch_BulkDeliveryNeverMovesPushedAtBackJson2))
	assert.Equal(t, "2026-01-01T00:00:00Z", pushedAt(), "an older view's pushed_at does not land")

	dispatch("workflow_job", "d-job-new", []byte(testDispatch_BulkDeliveryNeverMovesPushedAtBackJson3))
	assert.Equal(t, "2026-01-02T00:00:00Z", pushedAt(), "a newer view's pushed_at lands")
}

//go:embed testdata/test-dispatch-bulk-delivery-never-moves-pushed-at-back.json
var testDispatch_BulkDeliveryNeverMovesPushedAtBackJson string

//go:embed testdata/test-dispatch-bulk-delivery-never-moves-pushed-at-back-2.json
var testDispatch_BulkDeliveryNeverMovesPushedAtBackJson2 string

//go:embed testdata/test-dispatch-bulk-delivery-never-moves-pushed-at-back-3.json
var testDispatch_BulkDeliveryNeverMovesPushedAtBackJson3 string
