// Package reqtimeline is an in-memory ring of timed traffic events — incoming
// GitHub webhook deliveries and outgoing proxied GitHub requests — feeding the
// dashboard's "Timeline" chart. Every event carries its REAL measured duration
// (a webhook's receipt→dispatch-complete time, a proxied request's upstream
// round-trip); nothing is faked to zero or inflated for display.
//
// Like the request log and the rate meter it is deliberately IN-MEMORY (a live
// operational view, not an audit log — and a DB table would force a
// cache-nuking SchemaVersion bump for sub-day-ephemeral data): it resets on
// restart. It is bounded two ways — events older than the retention window
// (24h) are evicted lazily on write and on read, and a hard count cap (100k)
// drops the oldest as a memory backstop against a traffic flood. There is
// deliberately NO background goroutine or timer; laziness is the whole
// eviction story.
//
// Methods are nil-receiver-safe (the events.Recorder / ratemeter stance), so
// call sites never need a nil guard.
package reqtimeline

import (
	"sort"
	"sync"
	"time"
)

// Event kinds.
const (
	KindWebhook = "webhook" // an incoming GitHub webhook delivery
	KindRequest = "request" // an outgoing proxied GitHub request
)

// Defaults for New. Retention is the primary bound; the count cap is a coarse
// memory backstop only (≈100k events × ~200 B ≈ 20 MB worst case).
const (
	DefaultRetention = 24 * time.Hour
	DefaultMaxEvents = 100_000
)

// Event is one timed traffic event. Kind-specific fields are omitempty so a
// webhook event carries no request noise and vice versa; Disposition is shared
// (webhook: applied/invalidated/ignored/error — request: hit/miss/passthrough/
// write/error).
type Event struct {
	// ID is a monotonically increasing sequence number — the client's merge
	// key and the ?since= cursor.
	ID   uint64 `json:"id"`
	Kind string `json:"kind"`
	// Lane is the swimlane this event renders on: "⇐ <event type>" for
	// webhooks, "<METHOD> <route shape>" (normalizeRoute's bounded families)
	// for requests — never a per-URL lane.
	Lane string `json:"lane"`
	// Start is when handling/fetching began; DurMs is the real measured
	// duration in milliseconds (0 for a sub-millisecond event — still its
	// true rounding, never a fabricated instant).
	Start time.Time `json:"start"`
	DurMs int64     `json:"dur_ms"`

	Disposition string `json:"disposition,omitempty"`

	// Webhook fields.
	EventType  string `json:"event_type,omitempty"`
	Action     string `json:"action,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty"`
	Repo       string `json:"repo,omitempty"`

	// Request fields.
	Method string `json:"method,omitempty"`
	Route  string `json:"route,omitempty"`
	Status int    `json:"status,omitempty"`
	// Actor is the caller's principal/label key; ActorName its verified
	// display name when one is known (display-only, like the request log).
	Actor     string `json:"actor,omitempty"`
	ActorName string `json:"actor_name,omitempty"`
}

// end is the instant the event finished — the eviction clock. Events are
// recorded at completion, so the ring is ordered by end time.
func (e Event) end() time.Time {
	return e.Start.Add(time.Duration(e.DurMs) * time.Millisecond)
}

// Recorder is the bounded in-memory event ring. The zero value is NOT ready;
// use New. All methods are safe on a nil receiver (no-ops / empty snapshots).
type Recorder struct {
	mu        sync.Mutex
	retention time.Duration
	maxEvents int
	nextID    uint64
	// events[head:] are the live entries, ordered by insertion (≈ end time,
	// since events are recorded at completion) and therefore by ID. head is
	// advanced on eviction and the slice compacted periodically so the
	// backing array cannot grow unboundedly.
	events []Event
	head   int
	// now is the clock; injectable by tests.
	now func() time.Time
}

// New returns a Recorder with the default 24h retention and 100k count cap.
func New() *Recorder {
	return &Recorder{retention: DefaultRetention, maxEvents: DefaultMaxEvents, now: time.Now}
}

// RecordWebhook records one incoming webhook delivery with its real measured
// handling duration (receipt → dispatch complete).
func (r *Recorder) RecordWebhook(start time.Time, dur time.Duration, eventType, action, deliveryID, repo, disposition string) {
	if r == nil {
		return
	}
	if eventType == "" {
		eventType = "(unknown)"
	}
	r.record(Event{
		Kind:        KindWebhook,
		Lane:        "⇐ " + eventType,
		Start:       start,
		DurMs:       dur.Milliseconds(),
		EventType:   eventType,
		Action:      action,
		DeliveryID:  deliveryID,
		Repo:        repo,
		Disposition: disposition,
	})
}

// RecordRequest records one outgoing proxied GitHub request with its real
// measured upstream duration. route must already be a normalized route SHAPE
// (normalizeRoute), never a raw path — lanes stay bounded.
func (r *Recorder) RecordRequest(start time.Time, dur time.Duration, method, route string, status int, disposition, actorKey, actorName string) {
	if r == nil {
		return
	}
	r.record(Event{
		Kind:        KindRequest,
		Lane:        method + " " + route,
		Start:       start,
		DurMs:       dur.Milliseconds(),
		Method:      method,
		Route:       route,
		Status:      status,
		Disposition: disposition,
		Actor:       actorKey,
		ActorName:   actorName,
	})
}

func (r *Recorder) record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	e.ID = r.nextID
	r.events = append(r.events, e)
	r.evictLocked(r.now())
}

// evictLocked drops entries older than the retention window (by end time —
// the ring is end-ordered) and then enforces the count cap, oldest first.
// Compaction is deferred until the dead prefix is large, so eviction stays
// amortized O(1) per record instead of copying the ring on every write.
func (r *Recorder) evictLocked(now time.Time) {
	cutoff := now.Add(-r.retention)
	for r.head < len(r.events) && r.events[r.head].end().Before(cutoff) {
		r.head++
	}
	if over := (len(r.events) - r.head) - r.maxEvents; over > 0 {
		r.head += over
	}
	// Compact once the dead prefix dominates, so the backing array is reused
	// and evicted entries become collectable.
	if r.head > 1024 && r.head > len(r.events)/2 {
		live := copy(r.events, r.events[r.head:])
		clearTail := r.events[live:len(r.events)]
		for i := range clearTail {
			clearTail[i] = Event{}
		}
		r.events = r.events[:live]
		r.head = 0
	}
}

// Snapshot is one read of the ring: the events after the cursor, the current
// max ID (the client's next cursor), and the retention boundary.
type Snapshot struct {
	Events []Event
	// MaxID is the newest assigned event ID — the client's next ?since=
	// cursor. It keeps advancing even when every event has been evicted.
	MaxID uint64
	// RetentionStart is now-retention: nothing older is retained.
	RetentionStart time.Time
	Now            time.Time
}

// Snapshot returns the live events with ID > sinceID (the full retained window
// when sinceID == 0), evicting lazily first. The returned slice is a copy.
func (r *Recorder) Snapshot(sinceID uint64) Snapshot {
	if r == nil {
		return Snapshot{Events: []Event{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.evictLocked(now)
	live := r.events[r.head:]
	// Live entries are ID-ordered (insertion order); binary-search the cursor.
	i := sort.Search(len(live), func(i int) bool { return live[i].ID > sinceID })
	out := make([]Event, len(live)-i)
	copy(out, live[i:])
	return Snapshot{
		Events:         out,
		MaxID:          r.nextID,
		RetentionStart: now.Add(-r.retention),
		Now:            now,
	}
}
