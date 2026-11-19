// The dashboard's push channel: GET /api/stream. See docs/dashboard/live-stream.md.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// liveTopic is one dashboard data source. A set bit says "this changed".
type liveTopic uint32

const (
	topicCache liveTopic = 1 << iota
	topicRequests
	topicWebhooks
	topicTimeline
	topicRateLimit
	topicLatency
)

const topicAll = topicCache | topicRequests | topicWebhooks | topicTimeline | topicRateLimit | topicLatency

// liveTopicNames is the wire vocabulary, in bit order. The client keys on these names.
var liveTopicNames = []struct {
	bit  liveTopic
	name string
}{
	{topicCache, "cache"},
	{topicRequests, "requests"},
	{topicWebhooks, "webhooks"},
	{topicTimeline, "timeline"},
	{topicRateLimit, "ratelimit"},
	{topicLatency, "latency"},
}

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

type liveSub struct {
	pending atomic.Uint32
	wake    chan struct{}
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
		s.pending.Or(uint32(t))
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	h.mu.RUnlock()
}

func (h *LiveHub) subscribe() *liveSub {
	s := &liveSub{wake: make(chan struct{}, 1)}
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

// liveMessage is one SSE data frame.
type liveMessage struct {
	// Boot is set on the first frame only.
	Boot string `json:"boot,omitempty"`
	// Now is the server clock in epoch milliseconds.
	Now      int64         `json:"now"`
	Topics   []string      `json:"topics"`
	Requests *liveRequests `json:"requests,omitempty"`
}

// liveRequests is the request-log delta since the stream's cursor, oldest first.
type liveRequests struct {
	Events        []requestEvent   `json:"events"`
	Total         int64            `json:"total"`
	ByDisposition map[string]int64 `json:"by_disposition"`
	// Missed counts events the ring dropped before this stream read them.
	Missed uint64 `json:"missed,omitempty"`
}

// handleStream serves the push channel. Any signed-in user gets the cache
// topic. An admin gets every topic and the request-log delta.
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
	mask := topicCache
	if admin {
		mask = topicAll
	}

	// The cursor starts at the log's tip unless the client names where it stopped.
	cursor := d.reqlog.latestSeq()
	if v := r.URL.Query().Get("requests_since"); v != "" {
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

	sub := d.live.subscribe()
	defer d.live.unsubscribe(sub)

	send := func(m liveMessage) bool {
		m.Now = time.Now().UnixMilli()
		if m.Topics == nil {
			m.Topics = []string{}
		}
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
		sub.pending.Or(uint32(topicRequests))
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}

	heartbeat := time.NewTicker(liveHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.live.closed:
			return
		case <-heartbeat.C:
			if !send(liveMessage{}) {
				return
			}
			continue
		case <-sub.wake:
		}
		// Let the burst land, then report it once.
		select {
		case <-r.Context().Done():
			return
		case <-d.live.closed:
			return
		case <-time.After(liveFlushInterval):
		}
		bits := liveTopic(sub.pending.Swap(0)) & mask
		if bits == 0 {
			continue
		}
		msg := liveMessage{}
		for _, t := range liveTopicNames {
			if bits&t.bit != 0 {
				msg.Topics = append(msg.Topics, t.name)
			}
		}
		if bits&topicRequests != 0 {
			delta := d.reqlog.since(cursor, liveRequestBatch)
			cursor = delta.next
			msg.Requests = &liveRequests{Events: delta.events, Total: delta.total, ByDisposition: delta.byDisp, Missed: delta.missed}
			if delta.more {
				sub.pending.Or(uint32(topicRequests))
				select {
				case sub.wake <- struct{}{}:
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

// ttlValue caches one slow answer for a fixed time. It keeps a push-driven
// refetch from turning into a GitHub call per event.
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
