package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// Passthrough debouncing coalesces identical in-flight, unmodeled reads
// behind a caller-credential key. See docs/cache/three-tier-contract.md.

const (
	// debounceMaxBatches is the runaway backstop; past it requests forward uncoalesced.
	debounceMaxBatches = 1024

	// debounceMaxBodyBytes caps the buffered response; past it every waiter falls back to its own passthrough.
	debounceMaxBodyBytes = 8 << 20 // 8 MiB

	// debounceFetchTimeout bounds the batch's call on a context detached from every waiter.
	debounceFetchTimeout = 60 * time.Second

	// debounceHeader reports the hold time; debounceBatchHeader the batch size.
	debounceHeader      = "X-GSM-Debounce"
	debounceBatchHeader = "X-GSM-Debounce-Batch"
)

// DebounceMaxWindow caps PASSTHROUGH_DEBOUNCE; exported so config validation matches this cap.
const DebounceMaxWindow = 30 * time.Second

// Debouncer coalesces identical in-flight passthrough reads. The zero value is
// not usable; construct with NewDebouncer. A nil *Debouncer is inert (Wrap
// returns the handler unchanged), so tests and disabled deployments need no
// special casing.
type Debouncer struct {
	window   time.Duration
	reqlog   *requestLog
	timeline *reqtimeline.Recorder

	mu      sync.Mutex
	batches map[string]*debounceBatch
	stopped bool

	// stopCh is closed by Drain to answer every waiter now instead of waiting out its window.
	stopCh   chan struct{}
	stopOnce sync.Once

	// inflight lets Drain wait out running batches before shutdown.
	inflight sync.WaitGroup
}

// NewDebouncer returns a Debouncer holding eligible passthrough reads for
// window. A window <= 0 disables coalescing entirely (nil is returned, which
// Wrap treats as a no-op passthrough).
func NewDebouncer(window time.Duration) *Debouncer {
	if window <= 0 {
		return nil
	}
	return &Debouncer{
		window:  window,
		batches: make(map[string]*debounceBatch),
		stopCh:  make(chan struct{}),
	}
}

// attach wires the observability sinks built inside NewRouter, after cmd/server's own NewDebouncer call.
func (d *Debouncer) attach(reqlog *requestLog, timeline *reqtimeline.Recorder) {
	if d == nil {
		return
	}
	d.reqlog = reqlog
	d.timeline = timeline
}

// Window reports the configured hold, or 0 when debouncing is off.
func (d *Debouncer) Window() time.Duration {
	if d == nil {
		return 0
	}
	return d.window
}

// debounceBatch is one held request: the waiters share whatever res the fetch
// produces. res is written before done is closed, so a waiter that has
// received from done reads it safely.
type debounceBatch struct {
	done    chan struct{}
	joined  int // waiters that attached before the window closed; guarded by Debouncer.mu
	res     *bufferedResponse
	batchSz int // joined at close time, for the served headers
}

// bufferedResponse is one upstream answer captured whole so it can be replayed
// to every member of a batch.
type bufferedResponse struct {
	status int
	header http.Header
	body   []byte
}

// Wrap returns a handler that coalesces eligible requests before falling
// through to next (the GitHub passthrough proxy). Ineligible requests reach
// next unchanged and immediately.
func (d *Debouncer) Wrap(next http.Handler) http.Handler {
	if d == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := debounceKey(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		b := d.join(key, r, next)
		if b == nil {
			// Draining or batch map full: forward directly (coalescing is an optimization, never a gate).
			next.ServeHTTP(w, r)
			return
		}

		waitStart := time.Now()
		select {
		case <-b.done:
		case <-r.Context().Done():
			// This caller hung up; the detached fetch continues for everyone else.
			return
		}
		if b.res == nil {
			// Nothing replayable (oversized body): pay for our own call instead of serving a truncated one.
			next.ServeHTTP(w, r)
			return
		}
		d.recordServed(r)
		writeDebounced(w, b.res, time.Since(waitStart), b.batchSz)
	})
}

// join attaches r to the batch for key, starting one (and its fetch goroutine)
// if none is open. Returns nil when the request must be forwarded directly.
func (d *Debouncer) join(key string, r *http.Request, next http.Handler) *debounceBatch {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return nil
	}
	b := d.batches[key]
	if b == nil {
		if len(d.batches) >= debounceMaxBatches {
			return nil
		}
		b = &debounceBatch{done: make(chan struct{})}
		d.batches[key] = b
		// Detached clone: one shared credential backs every waiter, so no single disconnect cancels it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), debounceFetchTimeout)
		d.inflight.Add(1)
		go d.run(key, b, r.Clone(ctx), cancel, next)
	}
	b.joined++
	return b
}

// run waits out the window, closes the batch to new arrivals, makes the single
// upstream call, and publishes the result to every waiter.
func (d *Debouncer) run(key string, b *debounceBatch, req *http.Request, cancel context.CancelFunc, next http.Handler) {
	defer d.inflight.Done()
	defer cancel()
	defer close(b.done)

	timer := time.NewTimer(d.window)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-d.stopCh: // shutting down: answer the waiters now
	case <-req.Context().Done(): // the detached fetch budget expired
	}

	// Close the batch: a later arrival opens a fresh one rather than reusing this answer.
	d.mu.Lock()
	if d.batches[key] == b {
		delete(d.batches, key)
	}
	b.batchSz = b.joined
	d.mu.Unlock()

	bw := &bufferingWriter{header: make(http.Header), status: http.StatusOK, limit: debounceMaxBodyBytes}
	// Already charted by the proxy's own transport; recording it again would double-count batched calls.
	next.ServeHTTP(bw, req)

	if bw.overflow {
		slog.Warn("debounce: upstream response too large to share; waiters will refetch individually",
			"method", req.Method, "route", normalizeRoute(req.URL.Path), "limit_bytes", debounceMaxBodyBytes, "waiters", b.batchSz)
		return // res stays nil: every waiter falls back to its own passthrough
	}
	b.res = &bufferedResponse{status: bw.status, header: bw.header, body: bw.body.Bytes()}
	d.recordSaved(req, b.batchSz)
}

// Drain stops accepting new batches, cuts every pending window short so the
// waiters are answered immediately, and waits (up to timeout) for the in-flight
// fetches to finish. cmd/server calls it during shutdown alongside the
// freshness manager's and the notifier's drains.
func (d *Debouncer) Drain(timeout time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.stopOnce.Do(func() { close(d.stopCh) })

	done := make(chan struct{})
	go func() {
		d.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("debounce: drain timed out with batches still in flight", "timeout", timeout)
	}
}

// recordServed counts one inbound request answered from a shared batch.
func (d *Debouncer) recordServed(r *http.Request) {
	d.reqlog.addDebounced(r.Method, normalizeRoute(r.URL.Path), 1, 0)
}

// recordSaved counts calls avoided: n requests served by one call save n-1; a batch of one saves nothing.
func (d *Debouncer) recordSaved(r *http.Request, batchSz int) {
	if batchSz > 1 {
		d.reqlog.addDebounced(r.Method, normalizeRoute(r.URL.Path), 0, int64(batchSz-1))
	}
}

// debounceKey derives the coalescing key, reporting false when the request must not be debounced.
// see docs/cache/three-tier-contract.md
func debounceKey(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	if r.ContentLength > 0 {
		return "", false
	}
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "Range"} {
		if r.Header.Get(h) != "" {
			return "", false
		}
	}
	token := bearerToken(r)
	if token == "" {
		return "", false
	}
	sum := sha256.New()
	for _, part := range []string{
		r.Method,
		r.URL.RequestURI(),
		ghclient.Fingerprint(token),
		r.Header.Get("Accept"),
		r.Header.Get("X-GitHub-Api-Version"),
	} {
		_, _ = sum.Write([]byte(strconv.Itoa(len(part))))
		_, _ = sum.Write([]byte{':'})
		_, _ = sum.Write([]byte(part))
	}
	return hex.EncodeToString(sum.Sum(nil)), true
}

// writeDebounced replays a batch's captured response to one waiter. Upstream
// headers are ADDED (not assigned) exactly as httputil.ReverseProxy copies
// them, so a debounced response carries the same headers the streaming
// passthrough would have — including the CORS headers corsMiddleware already
// put on w. Response TRAILERS are not replayed; GitHub's REST API does not
// send any.
func writeDebounced(w http.ResponseWriter, res *bufferedResponse, waited time.Duration, batchSz int) {
	dst := w.Header()
	for k, vv := range res.header {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	dst.Set(debounceHeader, strconv.FormatInt(waited.Milliseconds(), 10)+"ms")
	dst.Set(debounceBatchHeader, strconv.Itoa(batchSz))
	w.WriteHeader(res.status)
	_, _ = w.Write(res.body)
}

// bufferingWriter captures a whole response for replay to several waiters, reporting overflow past limit
// rather than serving anyone a truncated body; writes keep reporting success so the proxy's copy loop drains cleanly.
type bufferingWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	limit       int
	overflow    bool
	wroteHeader bool
}

func (b *bufferingWriter) Header() http.Header { return b.header }

func (b *bufferingWriter) WriteHeader(code int) {
	if !b.wroteHeader {
		b.status = code
		b.wroteHeader = true
	}
}

func (b *bufferingWriter) Write(p []byte) (int, error) {
	b.wroteHeader = true
	if b.overflow {
		return len(p), nil // already abandoned; drain the rest
	}
	if b.body.Len()+len(p) > b.limit {
		b.overflow = true
		b.body.Reset() // release what we held; nobody will serve it
		return len(p), nil
	}
	return b.body.Write(p)
}

// Flush satisfies http.Flusher; buffering means there is nothing to flush until the batch completes.
func (b *bufferingWriter) Flush() {}
