// Forwarder posts every delivery this service applies to the engine's events path.

package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// forwardTimeout bounds the engine's apply of one forwarded delivery.
const forwardTimeout = 30 * time.Second

// delivery is one applied delivery that waits for the engine.
type delivery struct {
	id, eventType string
	body          []byte
}

// Forwarder hands each applied delivery to the engine on its own goroutine,
// in apply order.
type Forwarder struct {
	eng    http.Handler
	path   string
	secret string

	mu      sync.Mutex
	queue   []delivery
	busy    bool
	closed  bool
	wake    chan struct{}
	idle    chan struct{}
	stopped chan struct{}
}

type eventsHandler interface {
	http.Handler
	EventsPath() string
}

func newForwarder(eng eventsHandler, secret string) *Forwarder {
	idle := make(chan struct{})
	close(idle)
	return &Forwarder{
		eng:     eng,
		path:    eng.EventsPath(),
		secret:  secret,
		wake:    make(chan struct{}, 1),
		idle:    idle,
		stopped: make(chan struct{}),
	}
}

// NotifyIngest queues the delivery for the engine. It never blocks on the engine.
func (f *Forwarder) NotifyIngest(event webhook.Event, _ webhook.DispatchResult, _ time.Time) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		slog.Error("engine forward: a delivery arrived after shutdown began; the engine does not get it",
			"delivery", event.DeliveryID, "type", event.Type)
		return
	}
	f.queue = append(f.queue, delivery{id: event.DeliveryID, eventType: event.Type, body: event.Raw})
	if !f.busy {
		f.busy = true
		f.idle = make(chan struct{})
	}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Forwarder) start() { go f.run() }

func (f *Forwarder) run() {
	defer close(f.stopped)
	for {
		f.mu.Lock()
		if len(f.queue) == 0 {
			if f.busy {
				f.busy = false
				close(f.idle)
			}
			closed := f.closed
			f.mu.Unlock()
			if closed {
				return
			}
			<-f.wake
			continue
		}
		d := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		f.forward(d)
	}
}

// forward posts one delivery in process, signed as GitHub signs it, and logs any answer that is not a success.
func (f *Forwarder) forward(d delivery) {
	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.path, bytes.NewReader(d.body))
	if err != nil {
		slog.Error("engine forward: building the request failed", "delivery", d.id, "type", d.eventType, "error", err)
		return
	}
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write(d.body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", d.eventType)
	req.Header.Set("X-GitHub-Delivery", d.id)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := &statusWriter{header: http.Header{}}
	f.eng.ServeHTTP(w, req)
	if w.code >= 300 {
		slog.Error("engine forward: the engine refused a delivery; its routes keep their last state until the TTL",
			"delivery", d.id, "type", d.eventType, "status", w.code, "body", w.body.String())
	}
}

// drain stops new deliveries and waits for the queued ones. It reports false when timeout passed first.
func (f *Forwarder) drain(timeout time.Duration) bool {
	f.mu.Lock()
	f.closed = true
	left := len(f.queue)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-f.stopped:
		return true
	case <-t.C:
		slog.Error("engine forward: queued deliveries did not reach the engine before shutdown", "queued_at_drain", left)
		return false
	}
}

// Idle waits until every queued delivery reached the engine, or for timeout.
func (f *Forwarder) Idle(timeout time.Duration) bool {
	f.mu.Lock()
	idle := f.idle
	f.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

// statusWriter keeps the status and a capped body of an in-process answer.
type statusWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *statusWriter) Header() http.Header { return w.header }

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if room := 512 - w.body.Len(); room > 0 {
		w.body.Write(b[:min(len(b), room)])
	}
	return len(b), nil
}
