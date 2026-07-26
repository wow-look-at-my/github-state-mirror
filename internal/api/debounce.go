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

// Passthrough DEBOUNCING — request coalescing for the reads the cache
// deliberately cannot model (operator directive, 2026-07-26).
//
// The uncached routes are uncached for good reasons (see the passthrough
// reason vocabulary in requestlog.go): a filter like ?status=queued describes
// a set that churns with every run in the repo, so no snapshot of it is
// honest. But the callers poll them in tight fleet-wide sweeps, and N
// identical polls arriving inside a few seconds do not need N round trips to
// GitHub.
//
// So an eligible passthrough is HELD for a short window (default 5s) instead
// of being forwarded immediately. Every identical request arriving during the
// window joins the same batch; when the window closes, ONE upstream call is
// made and its response is served to every member. Two properties follow, and
// both are intended:
//
//  1. Upstream volume collapses to at most one call per (request, window).
//  2. Uncached endpoints become visibly SLOW. That is a feature, not a
//     regression — it prices the cost of an unmodelable read into the caller's
//     own latency and pushes consumers toward the cached shapes. The
//     X-GSM-Debounce headers make the charge legible rather than mysterious.
//
// THE SECURITY PROPERTY, which must never regress: a batch is keyed by the
// CALLER'S CREDENTIAL (a SHA-256 fingerprint of the bearer token), so a
// response is only ever shared between requests that presented the SAME
// token. The passthrough proxy has no reveal gate — it forwards the caller's
// Authorization and lets GitHub decide — so sharing one caller's answer with
// another's request would hand out data GitHub never agreed to show them.
// Identical token + identical request = provably identical answer; anything
// less than identical gets its own batch. Everything else that can change a
// response (the URL verbatim, Accept, the API version) is in the key too, and
// requests whose answer depends on caller state the key cannot capture
// (conditional and Range reads) are never debounced at all.
//
// Only idempotent GET reads are eligible. A mutation is never coalesced: two
// identical POSTs are two distinct intents, not one answer shared twice.

const (
	// debounceMaxBatches bounds the in-flight batch map. Each batch lives at
	// most one window plus its fetch, so real traffic never approaches this;
	// the cap is the runaway backstop for a caller minting endless distinct
	// URLs. When full, requests are forwarded directly (correct, just
	// uncoalesced) rather than queued behind a full map.
	debounceMaxBatches = 1024

	// debounceMaxBodyBytes caps the buffered upstream response. Fanning a
	// response out to N waiters means holding it in memory, so an unbounded
	// body must not be buffered. The cap matches the cached routes' absorb
	// limit; a response past it makes the batch unusable and every waiter
	// falls back to its own direct passthrough (correct, just uncoalesced).
	debounceMaxBodyBytes = 8 << 20 // 8 MiB

	// debounceFetchTimeout bounds the batch's own upstream call. The fetch
	// runs on a context DETACHED from every waiter (the freshness-fetch
	// doctrine): one caller hanging up must not cancel the answer the rest of
	// the batch is still waiting for.
	debounceFetchTimeout = 60 * time.Second

	// debounceHeader reports how long this response was held (e.g. "4998ms"),
	// and debounceBatchHeader how many requests shared it. Together they make
	// the cost of an uncached read legible to the caller that paid it.
	debounceHeader      = "X-GSM-Debounce"
	debounceBatchHeader = "X-GSM-Debounce-Batch"
)

// DebounceMaxWindow is the largest hold internal/config will accept for
// PASSTHROUGH_DEBOUNCE. Exported so the config parser and this package cannot
// drift apart on what counts as a plausible window (a fat-fingered "5m" would
// wedge every uncached read for five minutes).
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

	// stopCh is closed by Drain to cut every pending window sleep short: at
	// shutdown a batch's waiters are still on the wire, so the right move is
	// to fetch and answer them NOW, not to make shutdown wait out the window.
	stopCh   chan struct{}
	stopOnce sync.Once

	// inflight tracks running batches so Drain can wait them out before the
	// process tears down (the freshness.Manager / notify.Notifier pattern).
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

// attach wires the observability sinks. Separate from NewDebouncer because
// cmd/server constructs the Debouncer (it owns Drain) while the request log
// and timeline ring are built inside NewRouter.
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
			// Draining, or the batch map is full: forward directly. Always
			// correct — coalescing is an optimization, never a gate.
			next.ServeHTTP(w, r)
			return
		}

		waitStart := time.Now()
		select {
		case <-b.done:
		case <-r.Context().Done():
			// This caller hung up. The batch fetch is detached and continues
			// for everyone else; nothing to write.
			return
		}
		if b.res == nil {
			// The batch produced nothing replayable (an oversized body). Pay
			// for our own call rather than serving a truncated answer.
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
		// The batch's own request is a clone of the first arrival's on a
		// DETACHED context: every member presented the same credential (the
		// key proves it), so this one request speaks for all of them, and no
		// single waiter's disconnect may cancel it.
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

	// Close the batch: from here a new arrival opens a fresh one, so a
	// response can never be served to a request that arrived after the call
	// it is answering was already issued.
	d.mu.Lock()
	if d.batches[key] == b {
		delete(d.batches, key)
	}
	b.batchSz = b.joined
	d.mu.Unlock()

	bw := &bufferingWriter{header: make(http.Header), status: http.StatusOK, limit: debounceMaxBodyBytes}
	who := callerLabel(req)
	start := time.Now()
	next.ServeHTTP(bw, req)
	// The batch's mirror→GitHub leg is a real exchange and goes on the chart
	// as its own "upstream" event — exactly like a cached-route miss's fetch.
	// Without it the Timeline would show N long inbound bars and no sign that
	// only ONE call to GitHub happened inside them.
	d.timeline.RecordRequest(start, time.Since(start), req.Method, normalizeRoute(req.URL.Path), bw.status, dispUpstream, who.Key, who.Name)

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

// recordSaved counts the upstream calls this batch avoided: a batch of n
// served n requests with one call, so n-1 never happened. A batch of one saved
// nothing — it only paid the window — and reporting that honestly is the point
// (see the two-counter note on routeGroup).
func (d *Debouncer) recordSaved(r *http.Request, batchSz int) {
	if batchSz > 1 {
		d.reqlog.addDebounced(r.Method, normalizeRoute(r.URL.Path), 0, int64(batchSz-1))
	}
}

// debounceKey derives the coalescing key for a request, reporting false when
// the request must not be debounced at all.
//
// ELIGIBILITY (all required):
//   - GET. Reads only — a mutation is an intent, not a shared answer — and
//     GET is the only method GitHub's API answers idempotently. HEAD is
//     excluded too: it is rare here and its bodyless response would need its
//     own replay path.
//   - A bearer token. It is the key's security component; a tokenless request
//     is 401'd by the proxy without an upstream call anyway.
//   - No request body. A GET with one is pathological and unmodeled.
//   - No conditional or Range headers. If-None-Match / If-Modified-Since /
//     If-Match / If-Unmodified-Since turn the answer into a function of what
//     THIS caller already holds (a 304 against their etag), and Range makes it
//     a slice of it. Sharing one caller's 304 — or one caller's byte range —
//     with another is simply a wrong answer, so these are never coalesced.
//
// THE KEY covers everything that can change the response: the method, the
// request URI verbatim (path + raw query — two spellings of the same query
// just miss each other, which costs a coalesce, never correctness), the
// credential fingerprint, and the content-negotiation headers. Components are
// length-delimited before hashing so no concatenation of one field can imitate
// another, and the digest keeps the map key bounded whatever the caller sends.
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

// bufferingWriter captures a handler's whole response in memory so it can be
// replayed to several waiters. Past limit it stops storing and reports
// overflow: the partial bytes are useless, so the batch is abandoned rather
// than serving anyone a truncated body. Writes keep reporting success so the
// proxy's copy loop drains the upstream connection cleanly.
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

// Flush satisfies http.Flusher, which httputil.ReverseProxy probes for when
// streaming. Buffering means there is nothing to flush until the batch
// completes.
func (b *bufferingWriter) Flush() {}
