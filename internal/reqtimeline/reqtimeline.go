// Package reqtimeline is an in-memory ring of timed traffic events — incoming
// GitHub webhook deliveries and outgoing proxied GitHub requests — feeding the
// dashboard's "Timeline" chart. Every event carries its REAL measured duration
// (a webhook's receipt→dispatch-complete time, a proxied request's upstream
// round-trip); nothing is faked to or inflated for display.
//
// Like the request log and the rate meter it is deliberately IN-MEMORY (a live
// operational view, not an audit log — and a DB table would put sub-day-
// ephemeral data behind a cache-nuking schema change): it resets on
// restart. It is bounded ways — events older than the retention window
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
	KindWebhook = "webhook" // an incoming GitHub webhook delivery (any outcome)
	KindRequest = "request" // an HTTP exchange on the GitHub data plane (inbound request or upstream call)
	KindNotify  = "notify"  // an outbound subscriber-notification delivery attempt
)

// Defaults for New. Retention is the primary bound; the count cap is a coarse
// memory backstop only (≈100k events × ~ B ≈ MB worst case).
const (
	DefaultRetention = 24 * time.Hour
	DefaultMaxEvents = 100_000
)

// Event is timed traffic event. Kind-specific fields are omitempty so a
// webhook event carries no request noise and vice versa; Disposition is shared
// (webhook: applied/invalidated/ignored/error — request: hit/miss/passthrough/
// write/error).
//
// The `wire:` tags are this repo's whole half of the Timeline chart's columnar
// format: the column names and their encodings, declared on the fields they
// describe. js-snippets' timelinewire does the rest. Field order within a kind
// IS wire order, and the chart's SCHEMA literal must match — pinned by
// TestTimelineSchemaMatchesChart, since a mismatch encodes perfectly and only
// fails in the browser.
type Event struct {
	// ID: see docs/dashboard/timeline-ring.md.
	ID   uint64 `json:"id" wire:"id,deltau"`
	Kind string `json:"kind" wire:"kind,string"`
	// Lane: see docs/dashboard/timeline-ring.md.
	Lane string `json:"lane" wire:"lane,string"`
	// Start/DurMs: see docs/dashboard/timeline-ring.md.
	Start time.Time `json:"start" wire:"start,deltaz"`
	DurMs int64     `json:"dur_ms" wire:"dur,plain"`

	Disposition string `json:"disposition,omitempty" wire:"disposition,string"`

	// Webhook fields.
	EventType  string `json:"event_type,omitempty" wire:"event_type,string"`
	Action     string `json:"action,omitempty" wire:"action,string"`
	DeliveryID string `json:"delivery_id,omitempty" wire:"delivery_id,string"`
	Repo       string `json:"repo,omitempty" wire:"repo,string"`

	// Request fields.
	Method string `json:"method,omitempty" wire:"method,string"`
	Route  string `json:"route,omitempty" wire:"route,string"`
	Status int    `json:"status,omitempty" wire:"status,plain"`
	// Actor/ActorName: see docs/dashboard/timeline-ring.md.
	Actor     string `json:"actor,omitempty" wire:"actor,string"`
	ActorName string `json:"actor_name,omitempty" wire:"actor_name,string"`

	// Detail is a short free-form tooltip line; never bodies or secrets.
	Detail string `json:"detail,omitempty" wire:"detail,string"`

	// Notify fields: see docs/dashboard/timeline-ring.md.
	Target  string `json:"target,omitempty" wire:"target,string"`
	Attempt int    `json:"attempt,omitempty" wire:"attempt,plain"`
	Final   bool   `json:"final,omitempty" wire:"final,bits"`
}

// end is the instant the event finished — the eviction clock. Events are
// recorded at completion, so the ring is ordered by end time.
func (e Event) end() time.Time {
	return e.Start.Add(time.Duration(e.DurMs) * time.Millisecond)
}

// Recorder is the bounded in-memory event ring. The value is NOT ready;
// use New. All methods are safe on a nil receiver (no-ops / empty snapshots).
type Recorder struct {
	mu        sync.Mutex
	retention time.Duration
	maxEvents int
	nextID    uint64
	// events[head:]: see docs/dashboard/timeline-ring.md.
	events []Event
	head   int
	// now is the clock; injectable by tests.
	now func() time.Time
}

// New returns a Recorder with the default 24h retention and 100k count cap.
func New() *Recorder {
	return &Recorder{retention: DefaultRetention, maxEvents: DefaultMaxEvents, now: time.Now}
}

// RecordWebhook records incoming webhook delivery with its real measured
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

// RecordWebhookRejected records a delivery that was REJECTED before dispatch
// (bad/missing signature, unset secret, unreadable body, wrong method). The
// lane is the FIXED "⇐ (unverified)" — never derived from request headers,
// which are attacker-controlled on this path and would otherwise mint
// unbounded lanes. The claimed event type rides along as clamped tooltip
// detail only.
func (r *Recorder) RecordWebhookRejected(start time.Time, dur time.Duration, disposition, claimedType, deliveryID string) {
	if r == nil {
		return
	}
	detail := ""
	if claimedType != "" {
		detail = "claimed event: " + clampDisplay(claimedType)
	}
	r.record(Event{
		Kind:        KindWebhook,
		Lane:        "⇐ (unverified)",
		Start:       start,
		DurMs:       dur.Milliseconds(),
		Disposition: disposition,
		DeliveryID:  clampDisplay(deliveryID),
		Detail:      detail,
	})
}

// RecordNotify records outbound subscriber-notification delivery attempt
// with its real measured duration. Every attempt is a real request and gets
// its own event; final marks the terminal (success, or the last retry).
func (r *Recorder) RecordNotify(start time.Time, dur time.Duration, target string, status int, attempt int, final bool, disposition string) {
	if r == nil {
		return
	}
	r.record(Event{
		Kind:        KindNotify,
		Lane:        "⇒ notify",
		Start:       start,
		DurMs:       dur.Milliseconds(),
		Target:      clampDisplay(target),
		Status:      status,
		Attempt:     attempt,
		Final:       final,
		Disposition: disposition,
	})
}

// clampDisplay bounds an untrusted display string (rune-safe) so junk input
// can't bloat events; lanes never use these values.
func clampDisplay(s string) string {
	const maxDisplay = 64
	if len(s) <= maxDisplay {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxDisplay {
		return s
	}
	return string(runes[:maxDisplay]) + "…"
}

// RecordRequest records HTTP exchange on the GitHub data plane — an
// inbound data-API request the mirror served, or an upstream call the mirror
// made (cached-route fetch, reveal probe, ghclient exchange, login relay) —
// with its real measured duration. route must already be a normalized route
// SHAPE (normalizeRoute), never a raw path — lanes stay bounded.
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
// the ring is end-ordered) and then enforces the count cap, oldest.
// Compaction is deferred until the dead prefix is large, so eviction stays
// amortized O() per record instead of copying the ring on every write.
func (r *Recorder) evictLocked(now time.Time) {
	cutoff := now.Add(-r.retention)
	for r.head < len(r.events) && r.events[r.head].end().Before(cutoff) {
		r.head++
	}
	if over := (len(r.events) - r.head) - r.maxEvents; over > 0 {
		r.head += over
	}
	// Compact the dead prefix dominates, so the backing array is reused
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

// Snapshot is read of the ring: the events after the cursor, the current
// max ID (the client's next cursor), and the retention boundary.
type Snapshot struct {
	Events []Event
	// MaxID: see docs/dashboard/timeline-ring.md.
	MaxID uint64
	// RetentionStart is now-retention: nothing older is retained.
	RetentionStart time.Time
	Now            time.Time
}

// SnapshotRange returns the live events overlapping [from, to) — the async
// history read behind the chart's lazy backward loading. A `from` means
// the retention floor and a `to` means now, so SnapshotRange(,) is the
// whole window.
//
// It exists because a full window is 100k events, and the client cannot turn
// 100k events into chart rows without blowing its frame budget: the chart
// paints hour, asks for older ranges as the viewport reaches them, and
// pays for each in a bounded chunk. MaxID is still the ring's newest id (the
// live cursor is independent of which range was read).
func (r *Recorder) SnapshotRange(from, to time.Time) Snapshot {
	if r == nil {
		return Snapshot{Events: []Event{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.evictLocked(now)
	live := r.events[r.head:]
	if from.IsZero() {
		from = now.Add(-r.retention)
	}
	if to.IsZero() {
		to = now
	}
	// Live entries are end-ordered; see docs/dashboard/timeline-ring.md.
	i := sort.Search(len(live), func(i int) bool { return !live[i].end().Before(from) })
	out := make([]Event, 0, len(live)-i)
	for _, e := range live[i:] {
		// An event overlaps the range if it starts before the end of it.
		if e.Start.Before(to) {
			out = append(out, e)
		}
	}
	return Snapshot{
		Events:         out,
		MaxID:          r.nextID,
		RetentionStart: now.Add(-r.retention),
		Now:            now,
	}
}

// Snapshot returns the live events with ID > sinceID (the full retained window
// when sinceID ==), evicting lazily. The returned slice is a copy.
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
