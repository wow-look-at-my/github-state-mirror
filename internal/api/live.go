// The dashboard's push channel: GET /api/stream. See docs/dashboard/live-stream.md.

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/latency"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// liveTopic is one dashboard data source. A set bit says "this changed". A topic never reaches the page.
type liveTopic uint32

const (
	topicCache liveTopic = 1 << iota
	topicRequests
	topicWebhooks
	topicTimeline
	topicRateLimit
	topicLatency
)

const (
	// liveFlushInterval coalesces a burst of changes into one message.
	liveFlushInterval = 100 * time.Millisecond
	// liveHeartbeat is the idle keep-alive.
	liveHeartbeat = 5 * time.Second
	// liveRequestBatch bounds the request events in one message.
	liveRequestBatch = 1000
)

// LiveHub fans change signals out to the open dashboard streams.
type LiveHub struct {
	// boot identifies this process, so a client can tell a restart from a reconnect.
	boot string
	n    atomic.Int32
	mu   sync.RWMutex
	subs map[*liveSub]struct{}

	closed    chan struct{}
	closeOnce sync.Once
}

// liveSub hears the topics in mask. wake holds at most one token.
type liveSub struct {
	mask liveTopic
	wake chan struct{}
}

func NewLiveHub() *LiveHub {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &LiveHub{boot: hex.EncodeToString(b[:]), subs: make(map[*liveSub]struct{}), closed: make(chan struct{})}
}

// Close ends every open stream. Shutdown calls it, because a stream never ends by itself.
func (h *LiveHub) Close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() { close(h.closed) })
}

// publish never blocks. Recorders call it from hot paths.
func (h *LiveHub) publish(t liveTopic) {
	if h == nil || h.n.Load() == 0 {
		return
	}
	h.mu.RLock()
	for s := range h.subs {
		if s.mask&t == 0 {
			continue
		}
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	h.mu.RUnlock()
}

func (h *LiveHub) subscribe(mask liveTopic) *liveSub {
	s := &liveSub{mask: mask, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.n.Store(int32(len(h.subs)))
	h.mu.Unlock()
	return s
}

func (h *LiveHub) unsubscribe(s *liveSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.n.Store(int32(len(h.subs)))
	h.mu.Unlock()
}

// watch points the in-memory recorders at the hub. Call it before traffic starts.
func (h *LiveHub) watch(tl *reqtimeline.Recorder, meter *ratemeter.Store, lat *latency.Recorder) {
	tl.SetOnRecord(func(kind string) {
		if kind == reqtimeline.KindWebhook {
			h.publish(topicTimeline | topicWebhooks)
			return
		}
		h.publish(topicTimeline)
	})
	meter.SetOnObserve(func() { h.publish(topicRateLimit) })
	lat.SetOnObserve(func(kind string) {
		// A released write lock is the signal that cache state changed.
		if kind == database.KindWriteHold {
			h.publish(topicLatency | topicCache)
			return
		}
		h.publish(topicLatency)
	})
}

// liveMessage is one SSE data frame. An empty one is the heartbeat.
type liveMessage struct {
	// Boot is set on the first frame only.
	Boot string `json:"boot,omitempty"`
	// Now is the server clock in epoch milliseconds.
	Now      int64         `json:"now"`
	Requests *liveRequests `json:"requests,omitempty"`
	// View is the data of the tab the stream was opened for.
	View *liveView `json:"view,omitempty"`
}

// liveView is one push of a tab's data. Error replaces Data when the build failed.
type liveView struct {
	Name  string          `json:"name"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// liveRequests is the request-log delta since the stream's cursor, oldest first.
type liveRequests struct {
	Events        []requestEvent   `json:"events"`
	Total         int64            `json:"total"`
	ByDisposition map[string]int64 `json:"by_disposition"`
	// Missed counts events the ring dropped before this stream read them.
	Missed uint64 `json:"missed,omitempty"`
}

// handleStream serves the push channel. ?view= names the tab whose data the
// stream carries. An admin also gets the request-log delta on every tab.
func (d *dashboard) handleStream(w http.ResponseWriter, r *http.Request) {
	login, ok := d.auth.Session(r)
	if !ok {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok || d.live == nil {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	admin := d.auth.IsAdmin(login)
	q := r.URL.Query()
	feed, status, why := d.liveFeedFor(q, login, admin)
	if status != 0 {
		http.Error(w, why, status)
		return
	}

	// The cursor starts at the log's tip unless the client names where it stopped.
	cursor := d.reqlog.latestSeq()
	if v := q.Get("requests_since"); v != "" {
		since, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			http.Error(w, "bad requests_since", http.StatusBadRequest)
			return
		}
		cursor = min(since, cursor)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")

	// Request events go to an admin only. A nil channel never wakes the loop.
	var wake chan struct{}
	if admin {
		sub := d.live.subscribe(topicRequests)
		defer d.live.unsubscribe(sub)
		wake = sub.wake
	}

	// The tab's data is built off this loop, so a slow build holds no request event back.
	var views chan liveView
	if feed != nil {
		views = make(chan liveView)
		ctx, cancel := context.WithCancel(r.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			d.pumpView(ctx, feed, views)
		}()
		defer func() {
			cancel()
			<-done
		}()
	}

	send := func(m liveMessage) bool {
		m.Now = time.Now().UnixMilli()
		body, err := json.Marshal(m)
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(body); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	if !send(liveMessage{Boot: d.live.boot}) {
		return
	}
	if admin && cursor < d.reqlog.latestSeq() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	heartbeat := time.NewTicker(liveHeartbeat)
	defer heartbeat.Stop()
	for {
		msg := liveMessage{}
		select {
		case <-r.Context().Done():
			return
		case <-d.live.closed:
			return
		case <-heartbeat.C:
		case v := <-views:
			msg.View = &v
		case <-wake:
			// Let the burst land, then report it once.
			select {
			case <-r.Context().Done():
				return
			case <-d.live.closed:
				return
			case <-time.After(liveFlushInterval):
			}
			delta := d.reqlog.since(cursor, liveRequestBatch)
			if len(delta.events) == 0 && delta.missed == 0 {
				continue
			}
			cursor = delta.next
			msg.Requests = &liveRequests{Events: delta.events, Total: delta.total, ByDisposition: delta.byDisp, Missed: delta.missed}
			if delta.more {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
		if !send(msg) {
			return
		}
		heartbeat.Reset(liveHeartbeat)
	}
}

// pumpView builds one tab's data and hands each change to the stream. It
// builds once at the start, and again each time the feed's topic is raised.
// The feed sets the least time between builds.
func (d *dashboard) pumpView(ctx context.Context, feed *liveFeed, out chan<- liveView) {
	// Subscribe before the first build, so a change during it is not lost.
	sub := d.live.subscribe(feed.topic)
	defer d.live.unsubscribe(sub)
	var last []byte
	for {
		built := time.Now()
		v := liveView{Name: feed.name}
		data, err := feed.build(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("live view build failed", "view", feed.name, "error", err)
			v.Error = "the server could not read the data of this tab"
		} else if data != nil {
			if v.Data, err = json.Marshal(data); err != nil {
				slog.Error("live view encode failed", "view", feed.name, "error", err)
				v.Data, v.Error = nil, "the server could not encode the data of this tab"
			}
		}
		// A build that changed nothing sends nothing.
		body := append([]byte(v.Error+"\x00"), v.Data...)
		if (v.Error != "" || v.Data != nil) && !bytes.Equal(body, last) {
			select {
			case out <- v:
				last = body
			case <-ctx.Done():
				return
			case <-d.live.closed:
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-d.live.closed:
			return
		case <-sub.wake:
		}
		pace := time.NewTimer(max(liveFlushInterval, feed.every-time.Since(built)))
		select {
		case <-ctx.Done():
			pace.Stop()
			return
		case <-d.live.closed:
			pace.Stop()
			return
		case <-pace.C:
		}
		// The next build covers every change up to here.
		select {
		case <-sub.wake:
		default:
		}
	}
}

// ttlValue caches one slow answer for a fixed time. It keeps a pushed tab
// from making a GitHub call per event.
type ttlValue[T any] struct {
	mu  sync.Mutex
	val T
	at  time.Time
	ok  bool
}

// get returns the cached value while it is younger than ttl. Otherwise it
// calls fetch. A failed fetch is returned and never stored.
func (c *ttlValue[T]) get(ttl time.Duration, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && time.Since(c.at) < ttl {
		return c.val, nil
	}
	v, err := fetch()
	if err != nil {
		return v, err
	}
	c.val, c.at, c.ok = v, time.Now(), true
	return v, nil
}
