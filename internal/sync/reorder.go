package sync

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The reorder window: a short hold that lets deliveries for one subject apply

// reorderBuffer holds in-flight batches, one per subject.
type reorderBuffer struct {
	window time.Duration
	stats  *OrderingStats

	mu      sync.Mutex
	pending map[string]*subjectBatch
}

type bufferedDelivery struct {
	event    webhook.Event
	at       time.Time // the payload's own clock for this view
	arrived  time.Time
	done     chan webhook.DispatchResult
	sequence int // arrival order within the batch, for measuring reordering
}

type subjectBatch struct {
	items []*bufferedDelivery
}

func newReorderBuffer(window time.Duration, stats *OrderingStats) *reorderBuffer {
	stats.recordWindow(window)
	return &reorderBuffer{window: window, stats: stats, pending: map[string]*subjectBatch{}}
}

// admit holds an orderable delivery for its subject's window and returns the
// result of dispatching it in sorted position. A zero/negative window, or an
// unorderable delivery (no subject), dispatches immediately -- there is
// nothing to sort it against.
//
// dispatch is called on a background context: once a delivery is buffered its
// write must land even if the HTTP request that carried it goes away, and the
// caller may have stopped waiting.
func (b *reorderBuffer) admit(ctx context.Context, subject string, at time.Time, event webhook.Event, dispatch func(context.Context, webhook.Event) webhook.DispatchResult) (webhook.DispatchResult, bool) {
	if b == nil || b.window <= 0 || subject == "" {
		return webhook.DispatchResult{}, false
	}
	item := &bufferedDelivery{event: event, at: at, arrived: time.Now(), done: make(chan webhook.DispatchResult, 1)}

	b.mu.Lock()
	batch, existing := b.pending[subject]
	if !existing {
		batch = &subjectBatch{}
		b.pending[subject] = batch
	}
	item.sequence = len(batch.items)
	batch.items = append(batch.items, item)
	b.mu.Unlock()

	if !existing {
		// This delivery opened the window; it owns closing it.
		go b.flushAfter(subject, dispatch)
	}

	select {
	case res := <-item.done:
		return res, true
	case <-ctx.Done():
		// The caller gave up (a disconnect, or the handler's own deadline).
		// The batch still dispatches on its own context; there is simply
		// nobody left to tell.
		return webhook.DispatchResult{
			Event: event.Type, Action: event.Action, Repo: event.RepoFullName(),
			Disposition: webhook.DispIgnored, Detail: "caller went away while the delivery was held for reordering",
		}, true
	}
}

func (b *reorderBuffer) flushAfter(subject string, dispatch func(context.Context, webhook.Event) webhook.DispatchResult) {
	time.Sleep(b.window)

	b.mu.Lock()
	batch := b.pending[subject]
	delete(b.pending, subject)
	b.mu.Unlock()
	if batch == nil {
		return
	}

	// Oldest view first; SliceStable keeps arrival order among same-second (second-granular) timestamps.
	items := batch.items
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })

	moved := false
	for i, it := range items {
		if it.sequence != i {
			moved = true
		}
	}
	if moved {
		b.stats.recordReordered(subject, len(items))
	}

	ctx := context.Background()
	for _, it := range items {
		b.stats.recordHeld(time.Since(it.arrived))
		it.done <- dispatch(ctx, it.event)
	}
}
