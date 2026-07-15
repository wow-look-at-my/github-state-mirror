package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The dashboard's "Timeline" tab: a swimlane chart of incoming GitHub webhook
// deliveries (one lane per event type) and outgoing proxied GitHub requests
// (one lane per method + route shape), each with its REAL measured duration.
// The data lives in internal/reqtimeline — an in-memory, 24h/100k-bounded
// ring fed by exactly three seams:
//
//   - the webhook handler (via deliveryTimeline below): receipt→dispatch time
//     of every verified delivery;
//   - recordPassthrough (requestlog.go): every request the passthrough proxy
//     forwards, timed end-to-end including the upstream round-trip;
//   - fetchUpstream (respcache.go): every cached-route MISS's upstream fetch —
//     real GitHub round-trips a proxy-only hook would never see.
//
// ghclient's own background traffic (periodic refreshes, consistency checks,
// token mints it makes itself) is deliberately NOT recorded: it does not go
// "through the proxy", and the chart is about proxied traffic.

// deliveryTimeline adapts the reqtimeline recorder to the webhook package's
// DeliveryRecorder seam (internal/webhook must not import internal/reqtimeline
// — it stays a leaf package).
type deliveryTimeline struct {
	tl *reqtimeline.Recorder
}

func (d deliveryTimeline) RecordDelivery(event webhook.Event, result webhook.DispatchResult, receivedAt time.Time, duration time.Duration) {
	d.tl.RecordWebhook(receivedAt, duration, event.Type, event.Action, event.DeliveryID, event.RepoFullName(), result.Disposition)
}

// timelineDeliveryRecorder wraps a recorder for webhook.Handler, keeping the
// handler's nil fast-path when no recorder is configured.
func timelineDeliveryRecorder(tl *reqtimeline.Recorder) webhook.DeliveryRecorder {
	if tl == nil {
		return nil
	}
	return deliveryTimeline{tl: tl}
}

// timelineResponse is the GET /api/timeline payload.
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
// actor/tenant. ?since=<id> returns only events newer than that cursor (the
// tab's incremental poll); omitted or 0 returns the full retained window.
func (d *dashboard) handleTimeline(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	var since uint64
	if s := r.URL.Query().Get("since"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			http.Error(w, "bad since cursor", http.StatusBadRequest)
			return
		}
		since = v
	}
	snap := d.timeline.Snapshot(since)
	writeJSON(w, timelineResponse{
		Events:         snap.Events,
		MaxID:          snap.MaxID,
		RetentionStart: snap.RetentionStart.UTC().Format(time.RFC3339Nano),
		Now:            snap.Now.UTC().Format(time.RFC3339Nano),
	})
}
