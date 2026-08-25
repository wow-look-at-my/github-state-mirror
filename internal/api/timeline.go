package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/httpobs"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// requestStartKey carries the instant the router received a request, stamped
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

// The dashboard's "Timeline" tab: every exchange the mirror participates in,
// each with its real measured duration. A gap here is a bug.
// see docs/dashboard/request-visibility.md, docs/dashboard/timeline-ring.md

// Timeline-only dispositions for exchanges that are not inbound cache
// answers. Inbound events keep the request-log vocabulary (hit / miss /
// passthrough / write / error).
const (
	dispUpstream = "upstream" // the mirror→GitHub leg of a cached-route miss
	dispProbe    = "probe"    // a reveal-layer authorization probe
	dispInternal = "internal" // the mirror's own ghclient exchange
	dispRelay    = "relay"    // a github.com login relay
	dispLogin    = "login"    // a dashboard sign-in's own GitHub calls
)

// Carries the upstream call's kind from call site to transport observer.
type upstreamDispKey struct{}

// withUpstreamDisposition labels an outbound request for the chart.
func withUpstreamDisposition(ctx context.Context, disp string) context.Context {
	return context.WithValue(ctx, upstreamDispKey{}, disp)
}

// TimelineUpstreamObserver charts every call the API layer's own client makes
// -- cached-route miss fetches and reveal probes today, and anything added
// later without a line of its own, which is the point.
func TimelineUpstreamObserver(tl *reqtimeline.Recorder) httpobs.Observer {
	return func(req *http.Request, status int, start time.Time, dur time.Duration) {
		who := callerLabel(req)
		disp, _ := req.Context().Value(upstreamDispKey{}).(string)
		if disp == "" {
			disp = dispUpstream
		}
		if status == 0 {
			disp = DispError
		}
		tl.RecordRequest(start, dur, req.Method, normalizeRoute(req.URL.Path), status, disp, who.Key, who.Name)
	}
}

// TimelineProxyObserver charts the mirror→GitHub leg of the passthrough
// proxy, including the debounced batches that share one call between many
// waiters. Identity comes from the outbound request, which carries the
// inbound one's context, so it lands under the same label the request log
// used for the inbound side.
func TimelineProxyObserver(tl *reqtimeline.Recorder) httpobs.Observer {
	return func(req *http.Request, status int, start time.Time, dur time.Duration) {
		who := callerLabel(req)
		disp := dispUpstream
		if status == 0 {
			disp = DispError
		}
		tl.RecordRequest(start, dur, req.Method, normalizeRoute(req.URL.Path), status, disp, who.Key, who.Name)
	}
}

// Charts a sign-in's two GitHub calls; labeled "anonymous" (no principal yet).
func TimelineLoginObserver(tl *reqtimeline.Recorder) httpobs.Observer {
	return func(req *http.Request, status int, start time.Time, dur time.Duration) {
		disp := dispLogin
		if status == 0 {
			disp = DispError
		}
		tl.RecordRequest(start, dur, req.Method, normalizeRoute(req.URL.Path), status, disp, "anonymous", "")
	}
}

// Adapts to webhook.DeliveryRecorder (a leaf package).
type deliveryTimeline struct {
	tl *reqtimeline.Recorder
}

func (d deliveryTimeline) RecordDelivery(event webhook.Event, result webhook.DispatchResult, receivedAt time.Time, duration time.Duration) {
	switch result.Disposition {
	case webhook.DispUnverified, webhook.DispRejected:
		// Untrusted; fixed lane, claimed metadata rides as clamped detail.
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

// Empty is unset; unparseable is rejected, not silently treated as unset.
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
	// Newest id; pass back as ?since=.
	MaxID uint64 `json:"max_id"`
	// Ring's window floor (now-24h).
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

	// Columnar payload for the exact wire media type; readable JSON otherwise.
	addVary(w.Header(), "Accept")
	if wantsTimelineWire(r.Header.Get("Accept")) {
		wire, err := encodeTimelineV1(snap)
		if err != nil {
			// Only a mistagged row type reaches here; no request causes it.
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
