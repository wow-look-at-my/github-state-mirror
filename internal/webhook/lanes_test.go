package webhook

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
)

// laneDispatcher records the delivery ids it applied.
type laneDispatcher struct {
	mu      sync.Mutex
	ids     []string
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (d *laneDispatcher) Dispatch(_ context.Context, event Event) DispatchResult {
	if d.gate != nil {
		first := false
		d.once.Do(func() { first = true })
		if first {
			close(d.started)
			<-d.gate
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, event.DeliveryID)
	return DispatchResult{Disposition: DispApplied}
}

func (d *laneDispatcher) applied() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

var laneEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// record puts a delivery in the journal through the queue, received at laneEpoch plus seq seconds.
func record(t *testing.T, q *Queue, id, eventType string, seq int) {
	t.Helper()
	fresh, err := q.enqueue(context.Background(), Stored{
		DeliveryID: id, EventType: eventType, Body: []byte(`{}`),
		ReceivedAt: laneEpoch.Add(time.Duration(seq) * time.Second),
	})
	require.NoError(t, err)
	require.True(t, fresh)
}

func runLaneQueue(t *testing.T, q *Queue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	t.Cleanup(func() {
		cancel()
		q.Stopped(5 * time.Second)
	})
}

func TestLaneOf(t *testing.T) {
	for _, typ := range []string{"workflow_job", "workflow_run"} {
		assert.Equal(t, LaneBulk, LaneOf(typ), typ)
	}
	for _, typ := range []string{"push", "pull_request", "pull_request_review", "check_run", "check_suite", "status", "repository", "create", "delete", "label", "installation", "something_new"} {
		assert.Equal(t, LaneClient, LaneOf(typ), typ)
	}
}

// A client delivery recorded behind a flood of bulk deliveries is applied first.
func TestQueue_ClientDeliveryJumpsTheBulkFlood(t *testing.T) {
	d := &laneDispatcher{}
	q := NewQueue(&memJournal{}, d, nil)
	for i := range 40 {
		record(t, q, fmt.Sprintf("job-%02d", i), "workflow_job", i)
	}
	record(t, q, "pr", "pull_request", 100)

	runLaneQueue(t, q)
	require.True(t, q.Idle(5*time.Second))
	got := d.applied()
	require.Len(t, got, 41)
	assert.Equal(t, "pr", got[0], "the pull_request goes before every queued workflow_job")
	for i := range 40 {
		assert.Equal(t, fmt.Sprintf("job-%02d", i), got[i+1], "the bulk lane keeps arrival order")
	}
}

// A client delivery. That arrives while the worker is draining the bulk lane
// waits for the delivery in hand only, not for the rest of the bulk batch.
func TestQueue_ClientArrivingMidBulkDrainWaitsForOneDelivery(t *testing.T) {
	d := &laneDispatcher{gate: make(chan struct{}), started: make(chan struct{})}
	q := NewQueue(&memJournal{}, d, nil)
	for i := range 20 {
		record(t, q, fmt.Sprintf("job-%02d", i), "workflow_job", i)
	}
	runLaneQueue(t, q)
	<-d.started

	record(t, q, "status", "status", 100)
	close(d.gate)
	require.True(t, q.Idle(5*time.Second))

	got := d.applied()
	require.Len(t, got, 21)
	assert.Equal(t, []string{"job-00", "status"}, got[:2], "the status goes right after the job the worker was holding")
}

// Under a steady client stream the bulk lane still moves: one bulk delivery
// after every bulkEvery client deliveries.
func TestQueue_BulkLaneIsNotStarved(t *testing.T) {
	d := &laneDispatcher{}
	q := NewQueue(&memJournal{}, d, nil)
	for i := range 3 {
		record(t, q, fmt.Sprintf("job-%d", i), "workflow_run", i)
	}
	for i := range 20 {
		record(t, q, fmt.Sprintf("pr-%02d", i), "pull_request", 10+i)
	}

	runLaneQueue(t, q)
	require.True(t, q.Idle(5*time.Second))

	var want []string
	pr := 0
	takePRs := func(n int) {
		for range n {
			want = append(want, fmt.Sprintf("pr-%02d", pr))
			pr++
		}
	}
	takePRs(bulkEvery)
	want = append(want, "job-0")
	takePRs(bulkEvery)
	want = append(want, "job-1")
	takePRs(20 - 2*bulkEvery)
	want = append(want, "job-2")
	assert.Equal(t, want, d.applied())
}

// A repository delivery lands after every bulk delivery recorded before it,
// as in arrival order, while later bulk deliveries still wait behind it.
func TestQueue_RepositoryDeliveryDrainsOlderBulkFirst(t *testing.T) {
	d := &laneDispatcher{}
	q := NewQueue(&memJournal{}, d, nil)
	record(t, q, "job-old", "workflow_job", 1)
	record(t, q, "pr", "pull_request", 2)
	record(t, q, "run-old", "workflow_run", 3)
	record(t, q, "repo-renamed", "repository", 4)
	record(t, q, "job-new", "workflow_job", 5)

	runLaneQueue(t, q)
	require.True(t, q.Idle(5*time.Second))
	assert.Equal(t, []string{"pr", "job-old", "run-old", "repo-renamed", "job-new"}, d.applied())
}

// A repository delivery behind more than a batch of older bulk deliveries
// still waits for all of them, across journal reads.
func TestQueue_RepositoryDeliveryDrainsBulkAcrossReads(t *testing.T) {
	d := &laneDispatcher{}
	q := NewQueue(&memJournal{}, d, nil)
	n := pendingBatch + 10
	for i := range n {
		record(t, q, fmt.Sprintf("job-%03d", i), "workflow_job", i)
	}
	record(t, q, "repo", "repository", n+1)

	runLaneQueue(t, q)
	require.True(t, q.Idle(5*time.Second))
	got := d.applied()
	require.Len(t, got, n+1)
	assert.Equal(t, "repo", got[n], "the repository delivery goes after every older job")
}

// The worker's writes are marked background, so they yield the write lock to requests.
func TestQueue_AppliesWithBackgroundWrites(t *testing.T) {
	var sawBackground bool
	var mu sync.Mutex
	d := dispatcherFunc(func(ctx context.Context, _ Event) DispatchResult {
		mu.Lock()
		defer mu.Unlock()
		sawBackground = database.IsBackgroundWrite(ctx)
		return DispatchResult{Disposition: DispApplied}
	})
	q := NewQueue(&memJournal{}, d, nil)
	record(t, q, "pr", "pull_request", 1)
	runLaneQueue(t, q)
	require.True(t, q.Idle(5*time.Second))
	mu.Lock()
	defer mu.Unlock()
	assert.True(t, sawBackground)
}

type dispatcherFunc func(ctx context.Context, event Event) DispatchResult

func (f dispatcherFunc) Dispatch(ctx context.Context, event Event) DispatchResult {
	return f(ctx, event)
}
