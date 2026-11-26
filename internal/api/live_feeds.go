// What GET /api/stream carries for each tab. See docs/dashboard/live-stream.md.

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// liveFeed is the data of one tab on one stream.
type liveFeed struct {
	name string
	// topic is raised when the data changed.
	topic liveTopic
	// every is the least time between builds. Zero means liveFlushInterval.
	every time.Duration
	// build returns the data to push. Nil data means there is nothing new.
	build func(ctx context.Context) (any, error)
}

// liveTuning bounds what the pushed tabs cost. A pace is the least time
// between builds of a tab.
type liveTuning struct {
	// paceCache is the longest: each build of a cache tab runs SQL counts.
	paceCache     time.Duration
	paceRequests  time.Duration
	paceAggregate time.Duration
	paceEvents    time.Duration
	// timelineBatch bounds the events in one Timeline frame.
	timelineBatch int
}

var defaultLiveTuning = liveTuning{
	paceCache:     5 * time.Second,
	paceRequests:  2 * time.Second,
	paceAggregate: time.Second,
	paceEvents:    250 * time.Millisecond,
	timelineBatch: 5000,
}

// liveTune returns the dashboard's tuning. Only a test sets its own.
func (d *dashboard) liveTune() liveTuning {
	if d.liveTuning != nil {
		return *d.liveTuning
	}
	return defaultLiveTuning
}

// liveFeedFor picks the feed that ?view= names. A nil feed with a zero status
// is a stream that carries no tab. A non-zero status refuses the request.
func (d *dashboard) liveFeedFor(q url.Values, login string, admin bool) (*liveFeed, int, string) {
	view := q.Get("view")
	tune := d.liveTune()
	switch view {
	case "":
		return nil, 0, ""
	case "mine":
		return d.cacheFeed(view, login, admin), 0, ""
	case "all", "webhooks", "requests", "ratelimit", "latency", "timeline":
	default:
		return nil, http.StatusBadRequest, "unknown view"
	}
	if !admin {
		return nil, http.StatusForbidden, "forbidden: admin only"
	}
	switch view {
	case "all":
		return d.cacheFeed(view, login, admin), 0, ""
	case "webhooks":
		return d.webhooksFeed(), 0, ""
	case "requests":
		return &liveFeed{name: view, topic: topicRequests, every: tune.paceRequests, build: func(context.Context) (any, error) {
			// The events ride the stream's own cursor. A cursor past the tip leaves them out of this.
			return d.requestsSnapshot(math.MaxUint64, true, 0), nil
		}}, 0, ""
	case "ratelimit":
		return &liveFeed{name: view, topic: topicRateLimit, every: tune.paceAggregate, build: func(ctx context.Context) (any, error) {
			return d.rateLimits(ctx), nil
		}}, 0, ""
	case "latency":
		return &liveFeed{name: view, topic: topicLatency, every: tune.paceAggregate, build: func(context.Context) (any, error) {
			return d.latencySnapshot(), nil
		}}, 0, ""
	}
	// The Timeline feed starts when the chart names the event it holds.
	raw := q.Get("timeline_since")
	if raw == "" {
		return nil, 0, ""
	}
	since, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, "bad timeline_since"
	}
	return d.timelineFeed(since), 0, ""
}

func (d *dashboard) cacheFeed(scope, login string, admin bool) *liveFeed {
	return &liveFeed{name: scope, topic: topicCache, every: d.liveTune().paceCache, build: func(ctx context.Context) (any, error) {
		return d.cacheStats(ctx, login, admin, scope)
	}}
}

// liveWebhooks is one Webhooks frame: what changed since the frame before it.
// A nil pointer field means "no change".
type liveWebhooks struct {
	// Reset is set on the first frame of a stream. The page drops the rows it holds.
	Reset bool `json:"reset,omitempty"`
	// Window is how many rows the page keeps: the newest ones by ID.
	Window int `json:"window"`
	// Deliveries holds the rows that are new or changed, newest first.
	Deliveries []ghdata.WebhookDelivery `json:"deliveries,omitempty"`
	// Ordering carries the counters only. OrderingRecent carries the samples.
	Ordering             *syncpkg.OrderingSnapshot   `json:"ordering,omitempty"`
	OrderingRecent       *[]syncpkg.OutOfOrderSample `json:"ordering_recent,omitempty"`
	MissingSubscriptions *[]missingSubscription      `json:"missing_subscriptions,omitempty"`
}

// webhooksFeed pushes each delivery row once, and again when its outcome is amended.
func (d *dashboard) webhooksFeed() *liveFeed {
	// What this stream sent: a row's outcome by row ID, and the last encoding of each panel.
	sent := map[int64]string{}
	first := true
	var lastOrdering, lastRecent, lastMissing []byte

	// changed reports whether v differs from the last value sent, and remembers it.
	changed := func(last *[]byte, v any) bool {
		body, err := json.Marshal(v)
		if err != nil || bytes.Equal(body, *last) {
			return false
		}
		*last = body
		return true
	}

	build := func(ctx context.Context) (any, error) {
		rows, err := d.store.RecentWebhookDeliveries(ctx, webhookWindow)
		if err != nil {
			return nil, err
		}
		out := liveWebhooks{Reset: first, Window: webhookWindow}
		inWindow := make(map[int64]string, len(rows))
		for _, row := range rows {
			outcome := row.Disposition + "\x00" + row.Detail
			inWindow[row.ID] = outcome
			if prev, ok := sent[row.ID]; !ok || prev != outcome {
				out.Deliveries = append(out.Deliveries, row)
			}
		}
		sent = inWindow

		if d.ordering != nil {
			snap := d.ordering()
			recent := snap.Recent
			if recent == nil {
				recent = []syncpkg.OutOfOrderSample{}
			}
			snap.Recent = nil
			if changed(&lastOrdering, snap) {
				out.Ordering = &snap
			}
			if changed(&lastRecent, recent) {
				out.OrderingRecent = &recent
			}
		}
		missing := d.missingSubscriptions(ctx)
		if missing == nil {
			missing = []missingSubscription{}
		}
		if changed(&lastMissing, missing) {
			out.MissingSubscriptions = &missing
		}

		if !first && len(out.Deliveries) == 0 && out.Ordering == nil && out.OrderingRecent == nil && out.MissingSubscriptions == nil {
			return nil, nil
		}
		first = false
		return out, nil
	}
	return &liveFeed{name: "webhooks", topic: topicWebhooks, every: d.liveTune().paceEvents, build: build}
}

// liveTimeline is one Timeline frame: the ring's events after Since, in the
// columnar wire format that GET /api/timeline serves.
type liveTimeline struct {
	// Since is the cursor this frame continues from. The chart refuses a frame that skips ahead of its own.
	Since uint64 `json:"since"`
	// Wire is the columnar page in base64, because an SSE frame is text.
	Wire string `json:"wire"`
	// More says the batch limit cut this frame short and the rest follows.
	More bool `json:"more,omitempty"`
}

// timelineFeed pushes the ring's events after cursor.
func (d *dashboard) timelineFeed(cursor uint64) *liveFeed {
	tune := d.liveTune()
	feed := &liveFeed{name: "timeline", topic: topicTimeline, every: tune.paceEvents}
	feed.build = func(context.Context) (any, error) {
		snap := d.timeline.Snapshot(cursor)
		if cursor > snap.MaxID {
			// The ring is behind the chart: this is a new process, and its ids start again.
			cursor = 0
			snap = d.timeline.Snapshot(0)
		}
		if len(snap.Events) == 0 {
			return nil, nil
		}
		more := len(snap.Events) > tune.timelineBatch
		if more {
			snap.Events = snap.Events[:tune.timelineBatch]
			// The page's cursor must stop where this frame stops.
			snap.MaxID = snap.Events[len(snap.Events)-1].ID
			d.live.publish(topicTimeline)
		}
		wire, err := encodeTimelineV1(snap)
		if err != nil {
			return nil, err
		}
		out := liveTimeline{Since: cursor, Wire: base64.StdEncoding.EncodeToString(wire), More: more}
		cursor = snap.MaxID
		return out, nil
	}
	return feed
}
