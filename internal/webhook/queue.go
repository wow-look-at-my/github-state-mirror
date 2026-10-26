// Stored is a verified delivery as the journal keeps it: enough to parse and apply it again.

package webhook

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
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
	// Pending returns up to limit recorded deliveries of lane not yet applied, oldest first.
	Pending(ctx context.Context, lane Lane, limit int) ([]Stored, error)
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
	// bulkEvery is how many client-lane deliveries the worker applies in a row before it applies one bulk delivery.
	bulkEvery = 8
)

// Queue answers GitHub once a delivery is in the journal and applies it from
// one worker. Each lane is applied in arrival order; the client lane goes
// first (see pick).
type Queue struct {
	journal    Journal
	dispatcher Dispatcher
	recorder   DeliveryRecorder
	notifiers  []IngestNotifier

	wake chan struct{}

	mu sync.Mutex
	// gen counts fresh deliveries, so the worker can tell one arrived while it looked.
	gen uint64
	// clientGen counts fresh client-lane deliveries, so the worker can leave the bulk lane for one.
	clientGen uint64
	// clientRun is how many client-lane deliveries the worker applied since its last bulk one; the worker alone touches it.
	clientRun int
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
	if LaneOf(d.EventType) == LaneClient {
		q.clientGen++
	}
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

		worked, err := q.pass(ctx)
		if err != nil {
			slog.Error("webhook queue: reading the journal failed; recorded deliveries wait", "error", err)
			q.sleep(ctx, journalRetry)
			continue
		}
		if worked {
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

// pass reads both lanes and applies what it read, in the order pick chooses;
// worked is false when nothing was pending. It returns early, to read again,
// when pick needs deliveries this read did not hold.
func (q *Queue) pass(ctx context.Context) (worked bool, err error) {
	q.mu.Lock()
	clientGen := q.clientGen
	q.mu.Unlock()

	client, err := q.journal.Pending(ctx, LaneClient, pendingBatch)
	if err != nil {
		return false, err
	}
	bulk, err := q.journal.Pending(ctx, LaneBulk, pendingBatch)
	if err != nil {
		return false, err
	}
	if len(client) == 0 && len(bulk) == 0 {
		return false, nil
	}
	read := batches{client: client, bulk: bulk, clientFull: len(client) == pendingBatch, bulkFull: len(bulk) == pendingBatch, clientGen: clientGen}
	// The apply outlives a cancelled Run, and its writes yield the write lock to requests.
	applyCtx := database.WithBackgroundWrites(context.WithoutCancel(ctx))
	for len(read.client) > 0 || len(read.bulk) > 0 {
		if ctx.Err() != nil {
			return true, nil
		}
		d, ok := q.pick(&read)
		if !ok {
			return true, nil
		}
		q.apply(applyCtx, d)
	}
	return true, nil
}

// batches is one read of both lanes, consumed from the front by pick.
type batches struct {
	client, bulk []Stored
	// clientFull and bulkFull report a batch that filled pendingBatch, so the journal may hold more behind it.
	clientFull, bulkFull bool
	// clientGen is the client-lane count at the read.
	clientGen uint64
}

// pick takes the next delivery off both batches, or reports false when the
// journal must be read again first. The client lane goes first, except:
//   - a delivery that drainsBulkFirst waits for every older bulk delivery;
//   - after bulkEvery client deliveries in a row, one bulk delivery goes.
//
// Each lane keeps arrival order. The batches came from one read, so a batch
// that is exhausted while it was full may have more behind it in the journal.
// A client delivery recorded since the read is behind the bulk batch.
func (q *Queue) pick(b *batches) (Stored, bool) {
	takeClient := func() Stored {
		d := b.client[0]
		b.client = b.client[1:]
		q.clientRun++
		return d
	}
	takeBulk := func() Stored {
		d := b.bulk[0]
		b.bulk = b.bulk[1:]
		q.clientRun = 0
		return d
	}

	if len(b.client) > 0 {
		head := b.client[0]
		if drainsBulkFirst(head.EventType) {
			if len(b.bulk) > 0 && arrivedBefore(b.bulk[0], head) {
				return takeBulk(), true
			}
			if len(b.bulk) == 0 && b.bulkFull {
				return Stored{}, false
			}
			return takeClient(), true
		}
		if q.clientRun < bulkEvery || len(b.bulk) == 0 {
			return takeClient(), true
		}
		return takeBulk(), true
	}
	if b.clientFull {
		return Stored{}, false
	}
	q.mu.Lock()
	clientArrived := q.clientGen != b.clientGen
	q.mu.Unlock()
	if clientArrived {
		return Stored{}, false
	}
	return takeBulk(), true
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
