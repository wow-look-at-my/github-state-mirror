package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// A tab's data arrives on the stream when it opens, and again when it changes.
// The page never asks for it.
func TestLiveStream_PushesTheTabOnOpenAndOnChange(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	d.latency.Observe(KindWebhook, 5*time.Millisecond)

	lf := openLive(t, svc, srv.URL+"?view=latency", "PazerOP")
	var first latencyResponse
	lf.view("latency", &first)
	require.Len(t, first.Kinds, 1, "the first push of a stream is the tab's whole data")
	assert.Equal(t, int64(uint64(1)), first.Kinds[0].Count)
	assert.Equal(t, float64(GitHubDeliveryTimeoutMs), first.WebhookTimeoutMs)

	d.latency.Observe(KindWebhook, 7*time.Millisecond)
	var second latencyResponse
	lf.view("latency", &second)
	assert.Equal(t, int64(uint64(2)), second.Kinds[0].Count, "a change is pushed, with no request from the page")

	d.live.publish(topicLatency)
	lf.quiet(400*time.Millisecond, "a build that changed nothing must send nothing")

	d.meter.Observe("user:1", "", &http.Response{Header: http.Header{"X-Ratelimit-Limit": {"5000"}, "X-Ratelimit-Remaining": {"4999"}}})
	lf.quiet(400*time.Millisecond, "a change to another tab must not reach this stream")
}

func TestLiveStream_RefusesAViewTheCallerMayNotRead(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	for _, view := range []string{"all", "webhooks", "requests", "ratelimit", "latency", "timeline"} {
		assert.Equal(t, http.StatusForbidden, openLive(t, svc, srv.URL+"?view="+view, "octocat").status, view)
	}
	assert.Equal(t, http.StatusBadRequest, openLive(t, svc, srv.URL+"?view=nope", "PazerOP").status)
	assert.Equal(t, http.StatusBadRequest, openLive(t, svc, srv.URL+"?view=timeline&timeline_since=x", "PazerOP").status)

	// A non-admin gets its own cache tab and nothing else.
	lf := openLive(t, svc, srv.URL+"?view=mine", "octocat")
	var mine cacheResponse
	lf.view("mine", &mine)
	assert.Equal(t, "octocat", mine.Login)
	assert.False(t, mine.IsAdmin)

	d.reqlog.record(callerIdent{Key: "user:1"}, "GET", "/secret/path", DispHit)
	d.live.publish(topicWebhooks | topicTimeline | topicRateLimit | topicLatency | topicCache)
	lf.quiet(400*time.Millisecond, "request events and admin topics never reach a non-admin")
}

// The Webhooks tab gets each delivery row one time, and again when its outcome changes.
func TestLiveStream_WebhooksArriveAsDeltas(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	ctx := context.Background()
	var ordering atomic.Pointer[syncpkg.OrderingSnapshot]
	ordering.Store(&syncpkg.OrderingSnapshot{Ordered: 2})
	d.ordering = func() syncpkg.OrderingSnapshot { return *ordering.Load() }
	record := func(id string) {
		require.NoError(t, d.store.RecordWebhookDelivery(ctx, ghdata.WebhookDelivery{
			DeliveryID: id, EventType: "push", Repo: "o/r", Disposition: webhook.DispApplied, Detail: "applied " + id,
		}))
	}
	record("d1")
	record("d2")

	lf := openLive(t, svc, srv.URL+"?view=webhooks", "PazerOP")
	var f liveWebhooks
	lf.view("webhooks", &f)
	assert.True(t, f.Reset, "the first push of a stream replaces what the page holds")
	assert.Equal(t, webhookWindow, f.Window)
	require.Len(t, f.Deliveries, 2)
	assert.Equal(t, "d2", f.Deliveries[0].DeliveryID, "newest first")
	assert.Greater(t, f.Deliveries[0].ID, f.Deliveries[1].ID)
	require.NotNil(t, f.Ordering)
	assert.Equal(t, int64(2), f.Ordering.Ordered)
	require.NotNil(t, f.OrderingRecent)
	require.NotNil(t, f.MissingSubscriptions)

	// One new delivery is one row on the wire.
	record("d3")
	ordering.Store(&syncpkg.OrderingSnapshot{Ordered: 3})
	d.live.publish(topicWebhooks)
	f = liveWebhooks{}
	lf.view("webhooks", &f)
	assert.False(t, f.Reset)
	require.Len(t, f.Deliveries, 1, "only the new row is sent")
	assert.Equal(t, "d3", f.Deliveries[0].DeliveryID)
	require.NotNil(t, f.Ordering)
	assert.Equal(t, int64(3), f.Ordering.Ordered)
	assert.Nil(t, f.OrderingRecent, "a panel that did not change is not sent again")
	assert.Nil(t, f.MissingSubscriptions)

	// The engine amends the outcome of a delivery after the dispatcher logged it.
	require.NoError(t, d.store.AmendWebhookDelivery(ctx, "d1", webhook.DispError, "engine refused"))
	d.live.publish(topicWebhooks)
	f = liveWebhooks{}
	lf.view("webhooks", &f)
	require.Len(t, f.Deliveries, 1, "a row whose outcome changed is sent again")
	assert.Equal(t, "d1", f.Deliveries[0].DeliveryID)
	assert.Equal(t, webhook.DispError, f.Deliveries[0].Disposition)
	assert.Nil(t, f.Ordering)

	d.live.publish(topicWebhooks)
	lf.quiet(400*time.Millisecond, "a delivery that changed no row must send nothing")
}

// timelineFrame reads the next Timeline push and decodes its columnar page.
func timelineFrame(t *testing.T, lf *liveFrames) (liveTimeline, []reqtimeline.Event, uint64) {
	t.Helper()
	var f liveTimeline
	lf.view("timeline", &f)
	wire, err := base64.StdEncoding.DecodeString(f.Wire)
	require.NoError(t, err)
	events, header := decodeTimeline(t, wire)
	return f, events, header.MaxID
}

func TestLiveStream_TimelineContinuesFromTheCursorOfTheChart(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	rec := func(route string) {
		d.timeline.RecordRequest(time.Now(), time.Millisecond, "GET", route, 200, DispHit, "user:1", "")
	}
	rec("/a")
	rec("/b")
	rec("/c")

	// The chart has not read its first page, so it names no cursor and gets no events.
	idle := openLive(t, svc, srv.URL+"?view=timeline", "PazerOP")
	idle.next()
	rec("/d")
	idle.quiet(400*time.Millisecond, "the Timeline feed starts when the chart names its cursor")

	lf := openLive(t, svc, srv.URL+"?view=timeline&timeline_since=1", "PazerOP")
	f, events, maxID := timelineFrame(t, lf)
	assert.Equal(t, uint64(1), f.Since)
	require.Len(t, events, 3, "everything after the cursor arrives without a request")
	assert.Equal(t, "/b", events[0].Route)
	assert.Equal(t, uint64(4), maxID)

	rec("/e")
	f, events, maxID = timelineFrame(t, lf)
	assert.Equal(t, uint64(4), f.Since, "each frame continues from the frame before it")
	require.Len(t, events, 1)
	assert.Equal(t, "/e", events[0].Route)
	assert.Equal(t, uint64(5), maxID)
}

func TestLiveStream_TimelineBatchesALongCatchUp(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	was := liveTimelineBatch
	liveTimelineBatch = 2
	t.Cleanup(func() { liveTimelineBatch = was })
	for range 5 {
		d.timeline.RecordRequest(time.Now(), time.Millisecond, "GET", "/x", 200, DispHit, "user:1", "")
	}

	lf := openLive(t, svc, srv.URL+"?view=timeline&timeline_since=0", "PazerOP")
	var got []uint64
	for _, want := range []struct {
		since, maxID uint64
		more         bool
	}{{0, 2, true}, {2, 4, true}, {4, 5, false}} {
		f, events, maxID := timelineFrame(t, lf)
		assert.Equal(t, want.since, f.Since)
		assert.Equal(t, want.more, f.More)
		assert.Equal(t, want.maxID, maxID, "a cut frame moves the cursor of the chart only as far as it reached")
		for _, e := range events {
			got = append(got, e.ID)
		}
	}
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, got)
}

// A cursor past the newest id of the ring is from a process that ended. The ids start again.
func TestLiveStream_TimelineCursorFromAnOlderProcess(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	d.timeline.RecordRequest(time.Now(), time.Millisecond, "GET", "/new", 200, DispHit, "user:1", "")

	lf := openLive(t, svc, srv.URL+"?view=timeline&timeline_since=900", "PazerOP")
	f, events, _ := timelineFrame(t, lf)
	assert.Equal(t, uint64(0), f.Since)
	require.Len(t, events, 1)
	assert.Equal(t, "/new", events[0].Route)
}

// The Requests tab's counters and group tables are pushed. The events ride the stream's own cursor.
func TestLiveStream_RequestsSummaryCarriesNoEvents(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	d.reqlog.record(callerIdent{Key: "user:1"}, "GET", "/repos/o/r", DispHit)

	lf := openLive(t, svc, srv.URL+"?view=requests", "PazerOP")
	var snap requestLogSnapshot
	lf.view("requests", &snap)
	assert.Equal(t, int64(1), snap.Total)
	assert.NotEmpty(t, snap.Groups)
	assert.Empty(t, snap.Recent, "the summary must not carry the log a second time")
	assert.Equal(t, d.live.boot, snap.Boot)
}
