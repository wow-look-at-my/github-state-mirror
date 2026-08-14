package sync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The out-of-order gate: what runs before every dispatch.
//
// GitHub orders nothing, so this service does. Each delivery states the moment
// its view is from and the subject it is a view of (webhook.OrderOf); the
// watermark for that subject says whether anything newer has already been
// applied (ghdata.ClaimEventOrder). A superseded delivery does not write.
//
// This is the SECOND of two mechanisms, and the one with no distance limit.
// The first is the reorder window (reorder.go): deliveries for one subject
// arriving within a couple of seconds of each other are sorted by their own
// clocks and all applied, oldest first. That window cannot be long -- every
// delivery pays it in latency and GitHub's delivery timeout is single-digit
// seconds -- so anything reversed by more than it lands here instead, where
// the older view is refused rather than reordered.
//
// Refusing is not a lesser outcome for a snapshot: GitHub payloads are full
// views, not deltas, so applying only the newest one lands on the same final
// state that ordering them would have. The exception is a payload carrying
// facts the newer one never restates, which is handled explicitly rather than
// lost (see supersededStillAbsorbs).
//
// The numbers are kept (OrderingStats) rather than assumed: how often this
// fires, how late the losers are, and which subjects produce them. An operator
// reading "significant disruption" needs the distribution, not a boolean.

// OutOfOrderSampleLimit bounds the retained recent-sample ring. Counters are
// unbounded and cheap; samples are what an operator actually reads, and only
// the recent ones matter.
const OutOfOrderSampleLimit = 50

// OutOfOrderSample is one refused delivery, kept for the dashboard.
type OutOfOrderSample struct {
	At              time.Time `json:"at"`
	EventType       string    `json:"event_type"`
	Action          string    `json:"action,omitempty"`
	Repo            string    `json:"repo,omitempty"`
	Subject         string    `json:"subject"`
	DeliveryID      string    `json:"delivery_id,omitempty"`
	SupersededBy    string    `json:"superseded_by,omitempty"`
	LatenessSeconds float64   `json:"lateness_seconds"`
	StillAbsorbed   bool      `json:"still_absorbed"`
	ClockField      string    `json:"clock_field,omitempty"`
	EventTime       time.Time `json:"event_time"`
	WatermarkTime   time.Time `json:"watermark_time"`
}

// OrderingStats is the in-memory record of what the gate did. It is a live
// view, not an audit log (the requestLog stance): bounded, lock-guarded, and
// reset on restart.
type OrderingStats struct {
	mu sync.Mutex

	ordered     int64 // applied, at or ahead of the watermark
	superseded  int64 // refused: a newer view was already applied
	unorderable int64 // the payload states no clock this service can use
	failed      int64 // the watermark read/claim itself errored (applied anyway)

	// Lateness distribution of the refused ones, in the two bands that mean
	// different things operationally.
	withinGrace int64 // <= OutOfOrderGrace: ordinary delivery jitter
	beyondGrace int64 // > OutOfOrderGrace: a redelivery, or a real gap
	worst       time.Duration
	totalLate   time.Duration

	// What the reorder window did, as opposed to what the watermark did.
	// `reordered` counts BATCHES the window actually re-sorted -- the number
	// that says whether the hold is earning the latency it costs everything
	// else, rather than how often it merely held something.
	held      int64
	reordered int64
	totalHeld time.Duration

	window  time.Duration
	byEvent map[string]int64
	samples []OutOfOrderSample
}

// OutOfOrderGrace splits ordinary delivery jitter from something worth
// investigating. Deliveries reversed by less than this are GitHub's own
// asynchrony -- two events produced within a second of each other, sent over
// separate connections. Beyond it, a delivery is late enough that something
// held it: a redelivery from the failure log, a restart drain, a tunnel
// hiccup. Both are refused identically; the split exists so the dashboard can
// say which kind is happening.
const OutOfOrderGrace = 10 * time.Second

func NewOrderingStats() *OrderingStats {
	return &OrderingStats{byEvent: map[string]int64{}}
}

// recordHeld notes one delivery's time in the reorder window, and recordWindow
// the window it was configured with (reported so the dashboard's held/reordered
// numbers can be read against the latency they cost).
func (s *OrderingStats) recordHeld(d time.Duration) {
	s.bump(func() {
		s.held++
		s.totalHeld += d
	})
}

func (s *OrderingStats) recordWindow(w time.Duration) { s.bump(func() { s.window = w }) }

// recordReordered notes a batch the window actually re-sorted -- the only
// evidence that holding deliveries changed any outcome.
func (s *OrderingStats) recordReordered(subject string, size int) {
	s.bump(func() { s.reordered++ })
	slog.Info("webhook: reorder window re-sorted a batch into event order",
		"subject", subject, "deliveries", size)
}

// OrderingSnapshot is the stats as the API serves them.
type OrderingSnapshot struct {
	Ordered              int64              `json:"ordered"`
	Superseded           int64              `json:"superseded"`
	Unorderable          int64              `json:"unorderable"`
	Failed               int64              `json:"failed"`
	SupersededWithin     int64              `json:"superseded_within_grace"`
	SupersededBeyond     int64              `json:"superseded_beyond_grace"`
	GraceSeconds         float64            `json:"grace_seconds"`
	WorstLatenessSeconds float64            `json:"worst_lateness_seconds"`
	MeanLatenessSeconds  float64            `json:"mean_lateness_seconds"`
	ByEvent              map[string]int64   `json:"by_event,omitempty"`
	Recent               []OutOfOrderSample `json:"recent,omitempty"`

	// The reorder window's own numbers: how many deliveries it held, how many
	// BATCHES it actually re-sorted, and the mean hold. A held count with a
	// zero reordered count is the window costing latency and buying nothing.
	Held              int64   `json:"held"`
	Reordered         int64   `json:"reordered"`
	MeanHoldSeconds   float64 `json:"mean_hold_seconds"`
	ReorderWindowSecs float64 `json:"reorder_window_seconds"`
}

// Snapshot copies the stats out. Nil-receiver-safe: an unwired dispatcher
// reports zeros rather than panicking a dashboard request.
func (s *OrderingStats) Snapshot() OrderingSnapshot {
	if s == nil {
		return OrderingSnapshot{GraceSeconds: OutOfOrderGrace.Seconds()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := OrderingSnapshot{
		Ordered: s.ordered, Superseded: s.superseded, Unorderable: s.unorderable, Failed: s.failed,
		SupersededWithin: s.withinGrace, SupersededBeyond: s.beyondGrace,
		GraceSeconds: OutOfOrderGrace.Seconds(), WorstLatenessSeconds: s.worst.Seconds(),
		Held: s.held, Reordered: s.reordered, ReorderWindowSecs: s.window.Seconds(),
	}
	if s.held > 0 {
		out.MeanHoldSeconds = s.totalHeld.Seconds() / float64(s.held)
	}
	if s.superseded > 0 {
		out.MeanLatenessSeconds = s.totalLate.Seconds() / float64(s.superseded)
	}
	if len(s.byEvent) > 0 {
		out.ByEvent = make(map[string]int64, len(s.byEvent))
		for k, v := range s.byEvent {
			out.ByEvent[k] = v
		}
	}
	if len(s.samples) > 0 {
		out.Recent = make([]OutOfOrderSample, len(s.samples))
		copy(out.Recent, s.samples)
	}
	return out
}

func (s *OrderingStats) recordOrdered()     { s.bump(func() { s.ordered++ }) }
func (s *OrderingStats) recordUnorderable() { s.bump(func() { s.unorderable++ }) }
func (s *OrderingStats) recordFailed()      { s.bump(func() { s.failed++ }) }

func (s *OrderingStats) bump(f func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *OrderingStats) recordSuperseded(sample OutOfOrderSample) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.superseded++
	late := time.Duration(sample.LatenessSeconds * float64(time.Second))
	s.totalLate += late
	if late > s.worst {
		s.worst = late
	}
	if late > OutOfOrderGrace {
		s.beyondGrace++
	} else {
		s.withinGrace++
	}
	s.byEvent[sample.EventType]++
	s.samples = append(s.samples, sample)
	if len(s.samples) > OutOfOrderSampleLimit {
		s.samples = s.samples[len(s.samples)-OutOfOrderSampleLimit:]
	}
}

// checkOrder runs the gate for one delivery. It reports whether the delivery
// may write, and (when it may not) the outcome to return.
func (d *WebhookDispatcher) checkOrder(ctx context.Context, event webhook.Event) (superseded bool, out outcome) {
	order, ok := webhook.OrderOf(event)
	if !ok {
		// No clock in the payload. Applying unconditionally is the honest
		// answer -- refusing over a timestamp we never had would drop state --
		// and the count says how much of the stream this is.
		d.ordering.recordUnorderable()
		return false, outcome{}
	}
	verdict, err := d.store.ClaimEventOrder(ctx, order.Subject, order.At, event.DeliveryID, event.Type, time.Now())
	if err != nil {
		// The gate is not a reason to lose a delivery: a failed watermark
		// read applies the event exactly as before, loudly.
		slog.Error("webhook: event-order check failed; applying the delivery unordered",
			"subject", order.Subject, "delivery", event.DeliveryID, "error", err)
		d.ordering.recordFailed()
		return false, outcome{}
	}
	if !verdict.Superseded {
		d.ordering.recordOrdered()
		return false, outcome{}
	}

	stillAbsorbed := d.supersededStillAbsorbs(ctx, event)
	sample := OutOfOrderSample{
		At: time.Now().UTC(), EventType: event.Type, Action: event.Action, Repo: event.RepoFullName(),
		Subject: order.Subject, DeliveryID: event.DeliveryID, SupersededBy: verdict.PreviousDelivery,
		LatenessSeconds: verdict.Lateness.Seconds(), StillAbsorbed: stillAbsorbed,
		ClockField: order.Field, EventTime: order.At, WatermarkTime: verdict.Previous,
	}
	d.ordering.recordSuperseded(sample)
	slog.Warn("webhook: delivery arrived out of order; refusing to write stale state",
		"subject", order.Subject, "event", event.Type, "delivery", event.DeliveryID,
		"event_time", order.At, "watermark", verdict.Previous, "late_by", verdict.Lateness,
		"superseded_by", verdict.PreviousDelivery, "immutable_facts_absorbed", stillAbsorbed)

	detail := "a newer view of " + order.Subject + " was already applied (" + verdict.Lateness.Round(time.Second).String() + " late)"
	if stillAbsorbed {
		detail += "; its immutable commits were still absorbed"
	}
	return true, outcome{disposition: webhook.DispSuperseded, detail: detail}
}

// supersededStillAbsorbs takes the parts of a superseded delivery that CANNOT
// be stale, and reports whether it took any.
//
// This is the one thing a reordering buffer would have bought and refusal does
// not: a payload can carry facts the newer view never restates. A push is the
// only such payload today -- it carries up to 2,048 full commit objects, and a
// later push to the same branch does not repeat the earlier one's commits. A
// commit is immutable and content-addressed, so absorbing it out of order is
// not a stale write at all; dropping it would just mean fetching it back
// later.
//
// Everything else refused here is a whole-resource snapshot (a PR, a check
// run, a status context), where the newer view states everything the older one
// did.
func (d *WebhookDispatcher) supersededStillAbsorbs(ctx context.Context, event webhook.Event) bool {
	if event.Type != "push" {
		return false
	}
	payload, err := webhook.ParsePushPayload(event.Raw)
	if err != nil {
		return false
	}
	if len(payload.ChainedCommits()) == 0 {
		return false
	}
	d.absorbPushCommits(ctx, payload)
	return true
}
