// Stored is a verified delivery as the journal keeps it: enough to parse and apply it again.

package webhook

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Stored is a verified delivery as the journal keeps it: enough to parse and apply it again.
type Stored struct {
	DeliveryID string
	EventType  string
	Body       []byte
	ReceivedAt time.Time
}

// Journal durably records deliveries between receipt and apply.
type Journal interface {
	// Put records d unless a delivery with its id is already recorded; fresh is false for a repeat.
	Put(ctx context.Context, d Stored) (fresh bool, err error)
	// Pending returns up to limit recorded deliveries not yet applied, oldest first.
	Pending(ctx context.Context, limit int) ([]Stored, error)
	// MarkApplied records that the delivery has been applied.
	MarkApplied(ctx context.Context, deliveryID string, at time.Time) error
}

const (
	// pendingBatch is how many recorded deliveries the worker reads per pass.
	pendingBatch = 64
	// applyAttempts bounds how often a delivery whose apply reported an error is tried before it is set aside.
	applyAttempts = 3
	// journalRetry is the pause after the journal itself fails to answer.
	journalRetry = time.Second
)

// Queue answers GitHub once a delivery is in the journal and applies it from one worker, in arrival order. Deliveries recorded but not applied when the process stopped are
// applied when the worker starts.
type Queue struct {
	journal    Journal
	dispatcher Dispatcher
	recorder   DeliveryRecorder
	notifiers  []IngestNotifier

	wake chan struct{}

	mu sync.Mutex
	// gen counts fresh deliveries, so the worker can tell one arrived while it looked.
	gen uint64
	// idle is closed while the worker has nothing left to apply.
	idle chan struct{}
	// stopped is closed when Run returns.
	stopped chan struct{}
}

// NewQueue applies journal's deliveries through dispatcher. A non-nil recorder
// sees every delivery; notifiers are told about each applied one.
func NewQueue(journal Journal, dispatcher Dispatcher, recorder DeliveryRecorder, notifiers ...IngestNotifier) *Queue {
	return &Queue{
		journal:    journal,
		dispatcher: dispatcher,
		recorder:   recorder,
		notifiers:  notifiers,
		wake:       make(chan struct{}, 1),
		idle:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
}

// enqueue records a verified delivery and wakes the worker; fresh is false when it was already recorded.
func (q *Queue) enqueue(ctx context.Context, d Stored) (bool, error) {
	fresh, err := q.journal.Put(ctx, d)
	if err != nil || !fresh {
		return fresh, err
	}
	q.mu.Lock()
	q.gen++
	select {
	case <-q.idle:
		q.idle = make(chan struct{})
	default:
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true, nil
}

// Run applies recorded deliveries until ctx ends. The delivery in hand when
// it ends is finished; the rest stay recorded for the next start.
func (q *Queue) Run(ctx context.Context) {
	defer close(q.stopped)
	for ctx.Err() == nil {
		q.mu.Lock()
		gen := q.gen
		q.mu.Unlock()

		batch, err := q.journal.Pending(ctx, pendingBatch)
		if err != nil {
			slog.Error("webhook queue: reading the journal failed; recorded deliveries wait", "error", err)
			q.sleep(ctx, journalRetry)
			continue
		}
		for _, d := range batch {
			if ctx.Err() != nil {
				return
			}
			q.apply(context.WithoutCancel(ctx), d)
		}
		if len(batch) > 0 {
			continue
		}

		q.mu.Lock()
		if q.gen == gen {
			select {
			case <-q.idle:
			default:
				close(q.idle)
			}
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-q.wake:
		}
	}
}

func (q *Queue) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// apply dispatches one recorded delivery, retrying an error result, then
// marks it applied and tells the recorder and notifiers.
func (q *Queue) apply(ctx context.Context, d Stored) {
	event := ParseEvent(d.EventType, d.Body)
	event.DeliveryID = d.DeliveryID

	var result DispatchResult
	for attempt := 1; attempt <= applyAttempts; attempt++ {
		result = q.dispatcher.Dispatch(ctx, event)
		if result.Disposition != DispError {
			break
		}
		slog.Warn("webhook queue: apply reported an error", "type", event.Type, "delivery", event.DeliveryID, "attempt", attempt, "detail", result.Detail)
	}
	if result.Disposition == DispError {
		slog.Error("webhook queue: delivery set aside after repeated errors; its state arrives with the next delivery or refresh",
			"type", event.Type, "delivery", event.DeliveryID, "attempts", applyAttempts, "detail", result.Detail)
	}

	if err := q.journal.MarkApplied(ctx, d.DeliveryID, time.Now()); err != nil {
		slog.Error("webhook queue: marking a delivery applied failed; it will be applied again", "delivery", d.DeliveryID, "error", err)
		time.Sleep(journalRetry)
	}
	if q.recorder != nil {
		q.recorder.RecordDelivery(event, result, d.ReceivedAt, time.Since(d.ReceivedAt))
	}
	ingestedAt := time.Now()
	for _, n := range q.notifiers {
		if n != nil {
			n.NotifyIngest(event, result, ingestedAt)
		}
	}
}

// Idle waits until every recorded delivery has been applied, or for timeout; true when they all were.
func (q *Queue) Idle(timeout time.Duration) bool {
	q.mu.Lock()
	idle := q.idle
	q.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-q.stopped:
		return false
	case <-t.C:
		return false
	}
}

// Stopped waits for Run to return after its context ended, or for timeout; true when it returned.
func (q *Queue) Stopped(timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-q.stopped:
		return true
	case <-t.C:
		return false
	}
}
