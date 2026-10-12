package sync

import (
	"context"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

type kindCounter struct {
	mu gosync.Mutex
	n  map[string]int
}

func (c *kindCounter) Observe(kind string, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[kind]++
}

func (c *kindCounter) get(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[kind]
}

// A delivery's writes -- watermark, cache flushes, repo absorb, the apply,
// and its log row -- commit together: the write lock is taken once per
// delivery, not once per statement.
func TestDispatch_OneDeliveryTakesTheWriteLockOnce(t *testing.T) {
	obs := &kindCounter{n: map[string]int{}}
	db, err := database.OpenObserved(filepath.Join(t.TempDir(), "test.db"), obs)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := ghdata.NewStore(db)
	d := NewWebhookDispatcher(freshness.NewManager(freshness.NewStore(db)), store)
	ctx := context.Background()

	push := []byte(`{
		"ref": "refs/heads/main",
		"before": "1111111111111111111111111111111111111111",
		"after": "2222222222222222222222222222222222222222",
		"repository": {"name": "repo1", "full_name": "org1/repo1", "default_branch": "main", "pushed_at": 1767225600, "owner": {"login": "org1"}},
		"head_commit": {"id": "2222222222222222222222222222222222222222", "timestamp": "2026-01-01T00:00:00Z"},
		"commits": [{"id": "2222222222222222222222222222222222222222", "tree_id": "3333333333333333333333333333333333333333", "message": "m", "timestamp": "2026-01-01T00:00:00Z", "author": {"name": "a", "email": "a@x"}, "committer": {"name": "a", "email": "a@x"}}]
	}`)
	event := webhook.ParseEvent("push", push)
	event.DeliveryID = "d-push"

	before := obs.get(database.KindWriteWait)
	result := d.Dispatch(ctx, event)
	assert.Equal(t, 1, obs.get(database.KindWriteWait)-before, "one delivery, one admission to the write lock")
	assert.Equal(t, 1, obs.get(database.KindCommit), "one delivery, one commit")
	assert.Equal(t, webhook.DispApplied, result.Disposition, result.Detail)

	repo, err := store.GetRepo(ctx, "org1", "repo1")
	require.NoError(t, err, "the repo absorbed from the payload is committed")
	assert.Equal(t, "main", repo.DefaultBranch.String)
	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, deliveries, 1, "the delivery's log row commits with it")
	assert.Equal(t, "d-push", deliveries[0].DeliveryID)
}
