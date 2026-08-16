package sync

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The reorder window. These use a short window so the suite does not sit in
// it; the mechanism is identical at 2s.

func windowedDispatcher(t *testing.T, window time.Duration) (*WebhookDispatcher, *ghdata.Store) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := ghdata.NewStore(db)
	return NewWebhookDispatcherWindowed(freshness.NewManager(freshness.NewStore(db)), store, window), store
}

// The case the window exists for: the OLDER view arrives second, inside the
// window. Both apply, oldest first -- so the newer one's state is what
// survives, and the older one is not refused at all.
func TestReorder_OlderArrivingSecondIsSortedBackIntoPlace(t *testing.T) {
	dispatcher, store := windowedDispatcher(t, 150*time.Millisecond)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]webhook.DispatchResult, 2)
	// The newer view arrives first; the older one lands while the window is
	// still open.
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
			prPayloadAt(t, "edited", "open", "org1", "repo1", 5, "Newer", "2026-08-10T15:08:18Z")))
	}()
	time.Sleep(20 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[1] = dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
			prPayloadAt(t, "edited", "open", "org1", "repo1", 5, "Older", "2026-08-10T15:06:10Z")))
	}()
	wg.Wait()

	for i, res := range results {
		assert.NotEqual(t, webhook.DispSuperseded, res.Disposition,
			"result %d: inside the window nothing is refused -- they are ordered", i)
	}
	pr, err := store.GetPullRequest(ctx, "org1", "repo1", 5)
	require.NoError(t, err)
	assert.Equal(t, "Newer", pr.Title, "the newer view is applied last, so it is what remains")

	stats := dispatcher.Ordering()
	assert.Equal(t, int64(2), stats.Held)
	assert.Equal(t, int64(1), stats.Reordered, "the batch was genuinely re-sorted")
	assert.Equal(t, int64(0), stats.Superseded)
	assert.Greater(t, stats.MeanHoldSeconds, 0.0)
}

// Past the window the buffer cannot help, and the watermark takes over: the
// late view is refused rather than written. The two mechanisms cover different
// distances, and this is the seam between them.
func TestReorder_PastTheWindowTheWatermarkRefuses(t *testing.T) {
	dispatcher, store := windowedDispatcher(t, 30*time.Millisecond)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 6, "Newer", "2026-08-10T15:08:18Z")))
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 6, "Older", "2026-08-10T15:06:10Z")))

	assert.Equal(t, webhook.DispSuperseded, res.Disposition)
	pr, err := store.GetPullRequest(ctx, "org1", "repo1", 6)
	require.NoError(t, err)
	assert.Equal(t, "Newer", pr.Title)
	assert.Equal(t, int64(1), dispatcher.Ordering().Superseded)
}

// Batching is per SUBJECT: one busy subject's window must not make another's
// deliveries wait behind it.
func TestReorder_WindowsAreIndependentPerSubject(t *testing.T) {
	dispatcher, _ := windowedDispatcher(t, 120*time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	for _, number := range []int{11, 12, 13} {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
				prPayloadAt(t, "edited", "open", "org1", "repo1", n, "PR", "2026-08-10T15:08:18Z")))
		}(number)
	}
	wg.Wait()

	assert.Less(t, time.Since(start), 400*time.Millisecond,
		"three subjects must wait one window in parallel, not three in series")
	stats := dispatcher.Ordering()
	assert.Equal(t, int64(3), stats.Held)
	assert.Equal(t, int64(0), stats.Reordered, "each batch held one delivery; nothing to re-sort")
}

// An unorderable payload has nothing to be sorted against, so it must not pay
// the window at all.
func TestReorder_UnorderableDeliveryIsNotHeld(t *testing.T) {
	dispatcher, _ := windowedDispatcher(t, 2*time.Second)
	ctx := context.Background()

	body := mustJSON(t, map[string]any{
		"action": "created", "label": map[string]any{"name": "bug", "color": "ff0000"},
		"repository": map[string]any{"name": "repo1", "owner": map[string]any{"login": "org1"}, "full_name": "org1/repo1"},
	})
	start := time.Now()
	dispatcher.Dispatch(ctx, webhook.ParseEvent("label", body))

	assert.Less(t, time.Since(start), time.Second, "no clock, no ordering, no hold")
	assert.Equal(t, int64(0), dispatcher.Ordering().Held)
}

// A zero window is the no-buffer configuration: deliveries dispatch on
// arrival and only the watermark orders them.
func TestReorder_ZeroWindowDispatchesImmediately(t *testing.T) {
	dispatcher, _ := windowedDispatcher(t, 0)
	ctx := context.Background()

	start := time.Now()
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		prPayloadAt(t, "edited", "open", "org1", "repo1", 14, "PR", "2026-08-10T15:08:18Z")))
	assert.Equal(t, webhook.DispApplied, res.Disposition)
	assert.Less(t, time.Since(start), 100*time.Millisecond)
	assert.Equal(t, int64(0), dispatcher.Ordering().Held)
}
