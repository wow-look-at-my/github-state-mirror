// Package relay forwards each verified webhook delivery to extra targets. The
// GitHub App has one webhook URL, so a second consumer of the same deliveries
// gets them only from here. GitHub never retries a delivery and its failure
// log records the mirror's answer, not the relay's, so a relay failure is
// invisible unless this package reports it. See docs/webhooks/relay.md.
package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/httpobs"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// Defaults for the zero fields of Config.
const (
	DefaultQueueSize      = 1000
	DefaultRetryInterval  = 10 * time.Second
	DefaultAttemptTimeout = 10 * time.Second
	maxErrorLen           = 200
)

// ForwardedHeaders are the request headers a relayed delivery carries. The
// target verifies X-Hub-Signature-256 itself, with the same secret.
var ForwardedHeaders = []string{
	"X-GitHub-Event",
	"X-GitHub-Delivery",
	"X-GitHub-Hook-ID",
	"X-GitHub-Hook-Installation-Target-ID",
	"X-GitHub-Hook-Installation-Target-Type",
	"X-Hub-Signature",
	"X-Hub-Signature-256",
	"Content-Type",
	"User-Agent",
}

// Config configures a Relay. Zero numeric fields take the defaults above.
type Config struct {
	// Targets are absolute http or https URLs, already validated.
	Targets []string
	// QueueSize bounds each target's queue. A full queue drops the delivery.
	QueueSize int
	// RetryInterval is the fixed wait between attempts. It never grows.
	RetryInterval time.Duration
	// AttemptTimeout bounds one POST.
	AttemptTimeout time.Duration
	// Timeline charts every attempt. Nil charts nothing.
	Timeline *reqtimeline.Recorder
}

// TargetStats is one target's state on the Webhooks tab.
type TargetStats struct {
	Target            string `json:"target"`
	Queued            int    `json:"queued"`
	QueueCapacity     int    `json:"queue_capacity"`
	InFlight          int    `json:"in_flight"`
	Delivered         uint64 `json:"delivered"`
	Retried           uint64 `json:"retried"`
	PermanentlyFailed uint64 `json:"permanently_failed"`
	Dropped           uint64 `json:"dropped"`
	LastError         string `json:"last_error,omitempty"`
	LastErrorAt       string `json:"last_error_at,omitempty"`
}

type delivery struct {
	id     string
	header http.Header
	body   []byte
}

type target struct {
	url     string
	display string
	queue   chan delivery

	mu sync.Mutex
	// pending counts queued plus in-flight deliveries. Drain waits on it.
	pending     int
	inFlight    int
	abandoned   int
	delivered   uint64
	retried     uint64
	failed      uint64
	dropped     uint64
	lastError   string
	lastErrorAt time.Time
}

// Relay owns one queue and one worker per target. A nil *Relay relays
// nothing, so a caller with no targets needs no guard.
type Relay struct {
	targets       []*target
	client        *http.Client
	retryInterval time.Duration
	timeline      *reqtimeline.Recorder

	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup

	mu      sync.Mutex
	stopped bool
}

// New starts a worker per target. It returns nil for an empty target list.
func New(cfg Config) *Relay {
	if len(cfg.Targets) == 0 {
		return nil
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.AttemptTimeout <= 0 {
		cfg.AttemptTimeout = DefaultAttemptTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Relay{
		retryInterval: cfg.RetryInterval,
		timeline:      cfg.Timeline,
		ctx:           ctx,
		cancel:        cancel,
	}
	r.client = httpobs.Client(cfg.AttemptTimeout, r.observe)
	// A redirect is a wrong target URL. The relay reports it and does not follow it.
	r.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, raw := range cfg.Targets {
		t := &target{url: raw, display: Display(raw), queue: make(chan delivery, cfg.QueueSize)}
		r.targets = append(r.targets, t)
		r.workers.Add(1)
		go r.run(t)
	}
	return r
}

// Display renders a target URL without userinfo or query, which can carry credentials.
func Display(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// RelayDelivery queues a verified delivery for every target and returns at
// once. It copies only ForwardedHeaders. The body must not change afterwards.
func (r *Relay) RelayDelivery(header http.Header, body []byte) {
	if r == nil {
		return
	}
	d := delivery{id: header.Get("X-GitHub-Delivery"), header: http.Header{}, body: body}
	for _, name := range ForwardedHeaders {
		if v := header.Values(name); len(v) > 0 {
			d.header[http.CanonicalHeaderKey(name)] = append([]string(nil), v...)
		}
	}
	// r.mu makes the stopped check and the enqueue one step against Drain.
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.targets {
		if r.stopped {
			r.drop(t, d, "relay has shut down")
			continue
		}
		t.mu.Lock()
		select {
		case t.queue <- d:
			t.pending++
			t.mu.Unlock()
		default:
			t.mu.Unlock()
			r.drop(t, d, fmt.Sprintf("queue full (capacity %d)", cap(t.queue)))
		}
	}
}

func (r *Relay) drop(t *target, d delivery, why string) {
	slog.Error("webhook relay: delivery dropped", "delivery", d.id, "target", t.display, "reason", why)
	t.mu.Lock()
	t.dropped++
	t.setErrorLocked("dropped delivery " + d.id + ": " + why)
	t.mu.Unlock()
}

// Stats reports every target, in configuration order. Nil-safe.
func (r *Relay) Stats() []TargetStats {
	if r == nil {
		return nil
	}
	out := make([]TargetStats, 0, len(r.targets))
	for _, t := range r.targets {
		t.mu.Lock()
		s := TargetStats{
			Target:            t.display,
			Queued:            len(t.queue),
			QueueCapacity:     cap(t.queue),
			InFlight:          t.inFlight,
			Delivered:         t.delivered,
			Retried:           t.retried,
			PermanentlyFailed: t.failed,
			Dropped:           t.dropped,
			LastError:         t.lastError,
		}
		if !t.lastErrorAt.IsZero() {
			s.LastErrorAt = t.lastErrorAt.UTC().Format(time.RFC3339)
		}
		t.mu.Unlock()
		out = append(out, s)
	}
	return out
}

// Drain lets the workers empty their queues until timeout, then stops them.
// It logs each target's undelivered count and reports whether all were sent.
func (r *Relay) Drain(timeout time.Duration) bool {
	if r == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for r.pending() > 0 && time.Now().Before(deadline) {
		<-tick.C
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
	r.workers.Wait()

	clean := true
	for _, t := range r.targets {
		t.mu.Lock()
		left := len(t.queue) + t.abandoned
		t.mu.Unlock()
		if left > 0 {
			clean = false
			slog.Error("webhook relay: shutdown left deliveries undelivered; they are lost", "target", t.display, "undelivered", left)
		} else {
			slog.Info("webhook relay: drained", "target", t.display)
		}
	}
	return clean
}

func (r *Relay) pending() int {
	n := 0
	for _, t := range r.targets {
		t.mu.Lock()
		n += t.pending
		t.mu.Unlock()
	}
	return n
}

// run sends one target's deliveries in arrival order, one at a time.
func (r *Relay) run(t *target) {
	defer r.workers.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case d := <-t.queue:
			t.mu.Lock()
			t.inFlight = 1
			t.mu.Unlock()
			done := r.deliver(t, d)
			t.mu.Lock()
			t.inFlight = 0
			t.pending--
			if !done {
				t.abandoned++
			}
			t.mu.Unlock()
		}
	}
}

// deliver retries d on a fixed interval until a 2xx or a permanent 4xx. It
// returns false only when shutdown abandons the delivery.
func (r *Relay) deliver(t *target, d delivery) bool {
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			select {
			case <-r.ctx.Done():
				return false
			case <-time.After(r.retryInterval):
			}
			t.mu.Lock()
			t.retried++
			t.mu.Unlock()
		}
		status, err := r.post(t, d, attempt)
		switch {
		case err == nil && success(status):
			t.mu.Lock()
			t.delivered++
			t.mu.Unlock()
			return true
		case err == nil && permanent(status):
			slog.Error("webhook relay: target refused delivery permanently; not retrying",
				"delivery", d.id, "target", t.display, "status", status, "attempt", attempt)
			t.mu.Lock()
			t.failed++
			t.setErrorLocked(fmt.Sprintf("delivery %s: http %d (permanent)", d.id, status))
			t.mu.Unlock()
			return true
		}
		if r.ctx.Err() != nil {
			return false
		}
		msg := fmt.Sprintf("http %d", status)
		if err != nil {
			msg = err.Error()
		}
		slog.Warn("webhook relay: attempt failed; retrying",
			"delivery", d.id, "target", t.display, "attempt", attempt, "error", msg, "retry_in", r.retryInterval)
		t.mu.Lock()
		t.setErrorLocked(fmt.Sprintf("delivery %s attempt %d: %s", d.id, attempt, msg))
		t.mu.Unlock()
	}
}

func (t *target) setErrorLocked(msg string) {
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen]
	}
	t.lastError = msg
	t.lastErrorAt = time.Now()
}

func success(status int) bool { return status >= 200 && status < 300 }

// permanent is a 4xx that a retry cannot change.
func permanent(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

type attemptKey struct{}

type attemptInfo struct {
	target     string
	deliveryID string
	attempt    int
}

func (r *Relay) post(t *target, d delivery, attempt int) (int, error) {
	ctx := context.WithValue(r.ctx, attemptKey{}, attemptInfo{target: t.display, deliveryID: d.id, attempt: attempt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(d.body))
	if err != nil {
		return 0, err
	}
	req.Header = d.header.Clone()
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// observe charts each attempt from the transport, so no attempt escapes the Timeline.
func (r *Relay) observe(req *http.Request, status int, start time.Time, dur time.Duration) {
	info, _ := req.Context().Value(attemptKey{}).(attemptInfo)
	ok := success(status)
	disp := "failed"
	if ok {
		disp = "delivered"
	}
	r.timeline.RecordRelay(start, dur, info.target, info.deliveryID, status, info.attempt, ok || permanent(status), disp)
}
