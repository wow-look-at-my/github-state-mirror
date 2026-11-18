package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/latency"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// liveFrames reads the frames of one open /api/stream connection.
type liveFrames struct {
	t      *testing.T
	frames chan liveMessage
	cancel context.CancelFunc
	status int
}

// openLive connects to url as login ("" sends no session) and reads frames until the test ends.
func openLive(t *testing.T, svc *auth.Service, url, login string) *liveFrames {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	require.NoError(t, err)
	if login != "" {
		req.AddCookie(mintSession(t, svc, login))
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	lf := &liveFrames{t: t, frames: make(chan liveMessage, 64), cancel: cancel, status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		close(lf.frames)
		return lf
	}
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	go func() {
		defer resp.Body.Close()
		defer close(lf.frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var m liveMessage
			if json.Unmarshal([]byte(line), &m) != nil {
				return
			}
			lf.frames <- m
		}
	}()
	return lf
}

// next returns the next frame, or fails the test after a couple of seconds.
func (lf *liveFrames) next() liveMessage {
	lf.t.Helper()
	select {
	case m, ok := <-lf.frames:
		require.True(lf.t, ok, "the stream ended")
		return m
	case <-time.After(2 * time.Second):
		require.Fail(lf.t, "no frame arrived")
		return liveMessage{}
	}
}

// ended reports that the server closed the stream.
func (lf *liveFrames) ended() bool {
	for {
		select {
		case _, ok := <-lf.frames:
			if !ok {
				return true
			}
		case <-time.After(2 * time.Second):
			return false
		}
	}
}

// liveDashboard is a dashboard with only what /api/stream and /api/requests read.
func liveDashboard(t *testing.T) (*dashboard, *auth.Service, *httptest.Server) {
	t.Helper()
	svc := configuredAuth(t)
	hub := NewLiveHub()
	log := newRequestLog()
	log.live = hub
	d := &dashboard{auth: svc, reqlog: log, live: hub}
	srv := httptest.NewServer(http.HandlerFunc(d.handleStream))
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)
	return d, svc, srv
}

func TestLiveStream_RefusesAnonymous(t *testing.T) {
	_, svc, srv := liveDashboard(t)
	assert.Equal(t, http.StatusUnauthorized, openLive(t, svc, srv.URL, "").status)
}

func TestLiveStream_PushesRequestsToAdmin(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	d.reqlog.record(callerIdent{Key: "user:1"}, "GET", "/before", DispHit)

	lf := openLive(t, svc, srv.URL, "PazerOP")
	hello := lf.next()
	assert.Equal(t, d.live.boot, hello.Boot, "the first frame names the process")
	assert.Empty(t, hello.Topics)

	d.reqlog.record(callerIdent{Key: "user:1", Name: "octocat"}, "GET", "/repos/o/r", DispMiss)
	d.reqlog.record(callerIdent{Key: "user:1", Name: "octocat"}, "GET", "/repos/o/r", DispHit)

	m := lf.next()
	assert.Equal(t, []string{"requests"}, m.Topics)
	require.NotNil(t, m.Requests)
	require.Len(t, m.Requests.Events, 2, "a burst arrives as one frame, without the event recorded before the stream opened")
	assert.Equal(t, uint64(2), m.Requests.Events[0].Seq, "oldest first")
	assert.Equal(t, "/repos/o/r", m.Requests.Events[0].Path)
	assert.Equal(t, DispMiss, m.Requests.Events[0].Disposition)
	assert.Equal(t, uint64(3), m.Requests.Events[1].Seq)
	assert.Equal(t, int64(3), m.Requests.Total)
	assert.Equal(t, int64(2), m.Requests.ByDisposition[DispHit])
	assert.Positive(t, m.Now)
}

func TestLiveStream_ResumesFromCursor(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	for range 5 {
		d.reqlog.record(callerIdent{Key: "user:1"}, "GET", "/x", DispHit)
	}
	lf := openLive(t, svc, srv.URL+"?requests_since=3", "PazerOP")
	lf.next()
	m := lf.next()
	require.NotNil(t, m.Requests, "a client behind the log is caught up without a new request")
	require.Len(t, m.Requests.Events, 2)
	assert.Equal(t, uint64(4), m.Requests.Events[0].Seq)

	bad := openLive(t, svc, srv.URL+"?requests_since=nope", "PazerOP")
	assert.Equal(t, http.StatusBadRequest, bad.status)
}

func TestLiveStream_NonAdminGetsCacheOnly(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	lf := openLive(t, svc, srv.URL, "octocat")
	lf.next()

	d.reqlog.record(callerIdent{Key: "user:1"}, "GET", "/secret/path", DispHit)
	d.live.publish(topicWebhooks | topicTimeline | topicRateLimit | topicLatency)
	d.live.publish(topicCache)

	m := lf.next()
	assert.Equal(t, []string{"cache"}, m.Topics, "admin topics never reach a non-admin")
	assert.Nil(t, m.Requests, "request events never reach a non-admin")
}

func TestLiveStream_EndsWhenTheHubCloses(t *testing.T) {
	d, svc, srv := liveDashboard(t)
	lf := openLive(t, svc, srv.URL, "PazerOP")
	lf.next()
	d.live.Close()
	assert.True(t, lf.ended(), "shutdown must end an open stream")
}

// The recorders tell the hub what changed. Nothing else does.
func TestLiveHub_WatchMapsRecordersToTopics(t *testing.T) {
	hub := NewLiveHub()
	tl, meter, lat := reqtimeline.New(), ratemeter.New(), latency.New()
	hub.watch(tl, meter, lat)
	sub := hub.subscribe()
	defer hub.unsubscribe(sub)
	take := func() liveTopic { return liveTopic(sub.pending.Swap(0)) }

	tl.RecordRequest(time.Now(), time.Millisecond, "GET", "/x", 200, DispHit, "user:1", "")
	assert.Equal(t, topicTimeline, take())

	tl.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d1", "o/r", "applied")
	assert.Equal(t, topicTimeline|topicWebhooks, take())

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-RateLimit-Limit", "5000")
	resp.Header.Set("X-RateLimit-Remaining", "4999")
	meter.Observe("user:1", "", resp)
	assert.Equal(t, topicRateLimit, take())

	lat.Observe(database.KindQuery, time.Millisecond)
	assert.Equal(t, topicLatency, take(), "a read changes no cached state")

	lat.Observe(database.KindWriteHold, time.Millisecond)
	assert.Equal(t, topicLatency|topicCache, take(), "a finished write is what changes the cache tabs")
}

func TestLiveHub_NilAndIdleAreSafe(t *testing.T) {
	var hub *LiveHub
	hub.publish(topicCache)
	hub.Close()
	NewLiveHub().publish(topicCache)
	NewLiveHub().watch(nil, nil, nil)
}

func TestRequestLog_Since(t *testing.T) {
	l := newRequestLog()
	assert.Empty(t, l.since(0, 10).events)
	for range 5 {
		l.record(callerIdent{Key: "u"}, "GET", "/x", DispHit)
	}

	d := l.since(2, 2)
	require.Len(t, d.events, 2)
	assert.Equal(t, uint64(3), d.events[0].Seq)
	assert.Equal(t, uint64(4), d.next)
	assert.True(t, d.more, "the limit cut the read short")

	d = l.since(d.next, 2)
	require.Len(t, d.events, 1)
	assert.False(t, d.more)
	assert.Equal(t, uint64(5), d.next)

	d = l.since(99, 10)
	assert.Empty(t, d.events)
	assert.Equal(t, uint64(5), d.next, "a cursor past the tip comes back to it")
}

func TestRequestLog_SinceReportsWhatTheRingDropped(t *testing.T) {
	l := newRequestLog()
	for range requestLogRecentCap + 30 {
		l.record(callerIdent{Key: "u"}, "GET", "/x", DispHit)
	}
	d := l.since(10, 5)
	assert.Equal(t, uint64(20), d.missed, "events 11 to 30 left the ring unread")
	require.Len(t, d.events, 5)
	assert.Equal(t, uint64(31), d.events[0].Seq)
}

func TestDashboard_Requests_SinceAndLimit(t *testing.T) {
	d, svc, _ := liveDashboard(t)
	for range 300 {
		d.reqlog.record(callerIdent{Key: "u"}, "GET", "/x", DispHit)
	}
	get := func(query string) (int, requestLogSnapshot) {
		req := httptest.NewRequest("GET", "/api/requests"+query, nil)
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := httptest.NewRecorder()
		d.handleRequests(w, req)
		var snap requestLogSnapshot
		if w.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
		}
		return w.Code, snap
	}

	_, snap := get("")
	assert.Len(t, snap.Recent, 200, "the default stays small")
	assert.Equal(t, uint64(300), snap.Seq)
	assert.Equal(t, d.live.boot, snap.Boot)
	assert.Equal(t, requestLogRecentCap, snap.LogCap)

	_, snap = get("?limit=10000")
	assert.Len(t, snap.Recent, 300)
	assert.Equal(t, uint64(300), snap.Recent[0].Seq, "newest first")

	_, snap = get("?since=297")
	require.Len(t, snap.Recent, 3)
	assert.Equal(t, uint64(298), snap.Recent[2].Seq)
	assert.NotEmpty(t, snap.Groups, "a cursor read still carries the group counters")

	_, snap = get("?since=300")
	assert.Empty(t, snap.Recent)

	for _, q := range []string{"?since=x", "?limit=x", "?limit=0", "?limit=-1"} {
		code, _ := get(q)
		assert.Equal(t, http.StatusBadRequest, code, q)
	}
}

func TestTTLValue(t *testing.T) {
	var c ttlValue[int]
	calls := 0
	fetch := func() (int, error) { calls++; return calls, nil }

	v, err := c.get(time.Hour, fetch)
	require.NoError(t, err)
	assert.Equal(t, 1, v)
	v, _ = c.get(time.Hour, fetch)
	assert.Equal(t, 1, v, "a second read inside the window makes no call")
	v, _ = c.get(0, fetch)
	assert.Equal(t, 2, v, "an expired value is fetched again")

	_, err = c.get(0, func() (int, error) { return 0, errors.New("down") })
	require.Error(t, err)
	v, _ = c.get(time.Hour, fetch)
	assert.Equal(t, 2, v, "a failure is never stored over the last good value")
}
