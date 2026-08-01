package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// requestStartKey carries the instant the router received a request, stamped
// by stampRequestStart on EVERY inbound request so any record site can report
// a real end-to-end duration.
type requestStartKey struct{}

// stampRequestStart is the first router middleware: it stamps the receipt
// time into the request context. observeStatus reads it back so every
// inbound data-API event on the timeline carries the request's real
// end-to-end duration.
func stampRequestStart(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestStartKey{}, time.Now())))
	})
}

// requestStartFrom returns the router receipt stamp, if the request passed
// through stampRequestStart.
func requestStartFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(requestStartKey{}).(time.Time)
	return t, ok
}

// The dashboard's "Timeline" tab: a swimlane chart of EVERY exchange the
// mirror participates in, each with its REAL measured duration. A gap on
// this chart is a bug — nothing the mirror does is deliberately hidden (the
// operator's ruling, 2026-07-15). The data lives in internal/reqtimeline —
// an in-memory, 24h/100k-bounded ring fed by:
//
//   - the webhook handler (via deliveryTimeline below): receipt→completion of
//     EVERY delivery attempt, verified or not — rejected/unverified deliveries
//     record on a fixed "⇐ (unverified)" lane (headers on that path are
//     attacker-controlled and must never mint lanes);
//   - requestLog.observe/observeStatus (requestlog.go): every inbound
//     data-API request the mirror serves — hits, misses, passthroughs,
//     writes, errors — timed end-to-end from the router's receipt stamp;
//   - fetchUpstream (respcache.go): the mirror→GitHub leg of every
//     cached-route miss (disposition "upstream");
//   - probeRepoAccess (reveal.go): every reveal probe (disposition "probe");
//   - ghclient's transport observer (TimelineExchangeObserver below): every
//     call the mirror's own GitHub client makes — identity resolution, app
//     verification, token mints, fleet-refresh and consistency-check
//     GraphQL, rate-limit polls — one event per real attempt (disposition
//     "internal");
//   - relayGitHubLogin (oauth.go): the github.com login relays (disposition
//     "relay");
//   - the subscriber notifier (internal/notify): every outbound delivery
//     attempt on the "⇒ notify" lane.
//
// The dashboard's own UI endpoints (/api/*, the login pages, assets) are the
// one surface not charted: the chart polling itself would recursively fill
// the chart with the act of viewing it. That exclusion is stated here and in
// the docs — never silently.

// Timeline-only dispositions for exchanges that are not inbound cache
// answers. Inbound events keep the request-log vocabulary (hit / miss /
// passthrough / write / error).
const (
	dispUpstream = "upstream" // the mirror→GitHub leg of a cached-route miss
	dispProbe    = "probe"    // a reveal-layer authorization probe
	dispInternal = "internal" // the mirror's own ghclient exchange
	dispRelay    = "relay"    // a github.com login relay
)

// deliveryTimeline adapts the reqtimeline recorder to the webhook package's
// DeliveryRecorder seam (internal/webhook must not import internal/reqtimeline
// — it stays a leaf package).
type deliveryTimeline struct {
	tl *reqtimeline.Recorder
}

func (d deliveryTimeline) RecordDelivery(event webhook.Event, result webhook.DispatchResult, receivedAt time.Time, duration time.Duration) {
	switch result.Disposition {
	case webhook.DispUnverified, webhook.DispRejected:
		// Rejected before verification: nothing in the request is
		// trustworthy. Fixed lane; claimed metadata rides as clamped detail.
		d.tl.RecordWebhookRejected(receivedAt, duration, result.Disposition, event.Type, event.DeliveryID)
	default:
		d.tl.RecordWebhook(receivedAt, duration, event.Type, event.Action, event.DeliveryID, event.RepoFullName(), result.Disposition)
	}
}

// TimelineExchangeObserver adapts the timeline ring onto ghclient's
// transport-level exchange observer, so every call the mirror's own GitHub
// client makes is charted — per real attempt, under the same bounded route
// lanes the proxied traffic uses. Wired in cmd/server next to SetRateObserver.
func TimelineExchangeObserver(tl *reqtimeline.Recorder) ghclient.ExchangeObserver {
	return func(identity, name, method, path string, status int, start time.Time, dur time.Duration) {
		disp := dispInternal
		if status == 0 {
			// The exchange died before a response arrived — a real failure.
			disp = DispError
		}
		tl.RecordRequest(start, dur, method, normalizeRoute(path), status, disp, identity, name)
	}
}

// timelineDeliveryRecorder wraps a recorder for webhook.Handler, keeping the
// handler's nil fast-path when no recorder is configured.
func timelineDeliveryRecorder(tl *reqtimeline.Recorder) webhook.DeliveryRecorder {
	if tl == nil {
		return nil
	}
	return deliveryTimeline{tl: tl}
}

// parseUnixMs reads an optional unix-millisecond query parameter. An empty
// value is "unset" (zero time, ok); anything unparseable is rejected rather
// than silently treated as unset — a typo'd bound must not answer with the
// whole window.
func parseUnixMs(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, true
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms).UTC(), true
}

// timelineResponse is the READABLE encoding of the payload — what any caller
// that does not ask for the columnar media type by name receives. The chart
// never parses it (see the handler); it exists so the endpoint stays
// inspectable with curl and jq.
type timelineResponse struct {
	Events []reqtimeline.Event `json:"events"`
	// MaxID is the newest event ID — pass it back as ?since= to receive only
	// newer events on the next poll.
	MaxID uint64 `json:"max_id"`
	// RetentionStart is the ring's window floor (now − 24h): nothing older is
	// retained, so the chart can pin its history boundary there.
	RetentionStart string `json:"retention_start"`
	Now            string `json:"now"`
}

// handleTimeline returns the timed traffic events for the dashboard's
// Timeline chart. Admin-only, like /api/requests — it spans every
// actor/tenant. Three read shapes, all answering the same payload:
//
//	(no params)          the whole retained window
//	?since=<id>          only events newer than that cursor — the 5s live poll
//	?from=&to=<unix ms>  the events overlapping a time window — the chart's
//	                     async history loader, which is what keeps a first
//	                     paint from having to decode 24h of traffic to draw
//	                     one hour of it
//
// since and from/to are mutually exclusive: they answer different questions
// (what is NEW vs what was HAPPENING), and silently ANDing them would let a
// history request come back empty because the live cursor had moved past it.
func (d *dashboard) handleTimeline(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	var since uint64
	if s := q.Get("since"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			http.Error(w, "bad since cursor", http.StatusBadRequest)
			return
		}
		since = v
	}
	from, okFrom := parseUnixMs(q.Get("from"))
	to, okTo := parseUnixMs(q.Get("to"))
	if !okFrom || !okTo {
		http.Error(w, "bad from/to (unix milliseconds)", http.StatusBadRequest)
		return
	}
	if since != 0 && (!from.IsZero() || !to.IsZero()) {
		http.Error(w, "since and from/to are mutually exclusive", http.StatusBadRequest)
		return
	}

	snap := d.timeline.Snapshot(since)
	if !from.IsZero() || !to.IsZero() {
		snap = d.timeline.SnapshotRange(from, to)
	}

	// The chart asks for the columnar payload (timelinewire.go) by exact media
	// type; anything else — curl's */*, a browser, an operator with jq — gets
	// readable JSON.
	//
	// What makes a second encoding safe here is that the CLIENT refuses to
	// fall back: fetchDecoded sends only the wire type and THROWS on any other
	// content type. That is what removed the silent failure. Before, the chart
	// accepted whatever came back, so an Accept that drifted quietly took a
	// decode costing ~10x the frames with nothing failing; the defect was the
	// fallback, not the JSON. Serving JSON to a human debugging the endpoint
	// cannot reach the chart, and cannot degrade it if it somehow did.
	//
	// The columnar layout is versioned in the magic AND the media type, so a
	// CHANGE to it is v2 — never a third branch here.
	addVary(w.Header(), "Accept")
	if wantsTimelineWire(r.Header.Get("Accept")) {
		wire, err := encodeTimelineV1(snap)
		if err != nil {
			// Only a page that disagrees with timelineSchema gets here, which no
			// request can cause — it means the two were edited apart.
			slog.Error("timeline wire encode failed", "error", err)
			http.Error(w, "encode failed", http.StatusInternalServerError)
			return
		}
		writeBody(w, r, timelineWireType, wire)
		return
	}

	body, err := json.Marshal(timelineResponse{
		Events:         snap.Events,
		MaxID:          snap.MaxID,
		RetentionStart: snap.RetentionStart.UTC().Format(time.RFC3339Nano),
		Now:            snap.Now.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		slog.Warn("timeline json encode failed", "error", err)
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	writeBody(w, r, "application/json", body)
}
