package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingUpstream: see docs/testing/test-harness.md.
type countingUpstream struct {
	calls   int32
	body    func(r *http.Request) string
	status  int
	delay   time.Duration
	headers map[string]string
}

func (u *countingUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&u.calls, 1)
		if u.delay > 0 {
			time.Sleep(u.delay)
		}
		for k, v := range u.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		status := u.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if u.body != nil {
			_, _ = w.Write([]byte(u.body(r)))
			return
		}
		_, _ = w.Write([]byte(mustJSONString(map[string]any{"call": n})))
	})
}

func (u *countingUpstream) count() int { return int(atomic.LoadInt32(&u.calls)) }

// debounceReq builds an eligible request: GET with a bearer token.
func debounceReq(target, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// fireConcurrent sends n copies of the request built by mk through h, all in
// flight at, and returns their recorders every has completed.
func fireConcurrent(h http.Handler, n int, mk func(i int) *http.Request) []*httptest.ResponseRecorder {
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = httptest.NewRecorder()
			h.ServeHTTP(recs[i], mk(i))
		}(i)
	}
	wg.Wait()
	return recs
}

// TestDebounce_CoalescesIdenticalReads is the core promise: identical polls
// arriving inside the window cost upstream call, and every of them is
// served that same response.
func TestDebounce_CoalescesIdenticalReads(t *testing.T) {
	u := &countingUpstream{}
	d := NewDebouncer(80 * time.Millisecond)
	t.Cleanup(func() { d.Drain(2 * time.Second) })
	h := d.Wrap(u.handler())

	const n = 6
	recs := fireConcurrent(h, n, func(int) *http.Request {
		return debounceReq("/repos/o/r/actions/runs?status=queued&per_page=100", "tok-a")
	})

	assert.Equal(t, 1, u.count(), "%d identical polls must cost exactly one upstream call", n)
	for i, w := range recs {
		assert.Equal(t, http.StatusOK, w.Code, "waiter %d", i)
		assert.Equal(t, `{"call":1}`, w.Body.String(), "waiter %d got the shared response", i)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "upstream headers are replayed")
		assert.Equal(t, strconv.Itoa(n), w.Header().Get(debounceBatchHeader))
		assert.NotEmpty(t, w.Header().Get(debounceHeader), "the hold is reported to the caller")
	}
}

// TestDebounce_NeverSharesAcrossCredentials is THE security test. The
// passthrough has no reveal gate — GitHub decides per token — so callers
// must never be served each other's answer, however identical their requests
// look. Different tokens = different batches = different upstream calls.
func TestDebounce_NeverSharesAcrossCredentials(t *testing.T) {
	u := &countingUpstream{body: func(r *http.Request) string {
		return mustJSONString(map[string]any{
			"seen_token": strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		})
	}}
	d := NewDebouncer(80 * time.Millisecond)
	t.Cleanup(func() { d.Drain(2 * time.Second) })
	h := d.Wrap(u.handler())

	// Same URL, same instant, different credentials.
	tokens := []string{"tok-alice", "tok-bob", "tok-alice", "tok-bob"}
	recs := fireConcurrent(h, len(tokens), func(i int) *http.Request {
		return debounceReq("/repos/private-org/secret/actions/runs?status=queued", tokens[i])
	})

	assert.Equal(t, 2, u.count(), "one upstream call per DISTINCT credential, never one for both")
	for i, w := range recs {
		assert.Equal(t, mustJSONString(map[string]any{"seen_token": tokens[i]}),
			w.Body.String(), "request %d must be answered with ITS OWN credential's response", i)
	}
}

// TestDebounce_KeyedByRequestShape: anything that can change the answer splits
// the batch. Same credential, different request => its own upstream call.
func TestDebounce_KeyedByRequestShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b func() *http.Request
	}{{
		name: "different query",
		a:    func() *http.Request { return debounceReq("/repos/o/r/actions/runs?status=queued", "tok") },
		b:    func() *http.Request { return debounceReq("/repos/o/r/actions/runs?status=in_progress", "tok") },
	}, {
		name: "different path",
		a:    func() *http.Request { return debounceReq("/repos/o/r1/actions/runs?status=queued", "tok") },
		b:    func() *http.Request { return debounceReq("/repos/o/r2/actions/runs?status=queued", "tok") },
	}, {
		name: "different Accept",
		a:    func() *http.Request { return debounceReq("/repos/o/r/x", "tok") },
		b: func() *http.Request {
			r := debounceReq("/repos/o/r/x", "tok")
			r.Header.Set("Accept", "application/vnd.github.raw")
			return r
		},
	}, {
		name: "different API version",
		a:    func() *http.Request { return debounceReq("/repos/o/r/x", "tok") },
		b: func() *http.Request {
			r := debounceReq("/repos/o/r/x", "tok")
			r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			return r
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			u := &countingUpstream{}
			d := NewDebouncer(60 * time.Millisecond)
			t.Cleanup(func() { d.Drain(2 * time.Second) })
			h := d.Wrap(u.handler())

			reqs := []func() *http.Request{tc.a, tc.b, tc.a, tc.b}
			fireConcurrent(h, len(reqs), func(i int) *http.Request { return reqs[i]() })
			assert.Equal(t, 2, u.count(), "the two distinct shapes must not share a batch")
		})
	}
}

// TestDebounce_IneligibleRequestsForwardImmediately: the exclusions are not
// just uncoalesced, they are UNDELAYED — an ineligible request must not pay
// the window. Writes are excluded because identical POSTs are intents;
// conditional and Range reads because their answer depends on caller state the
// key cannot capture (a against someone else's etag is a wrong answer).
func TestDebounce_IneligibleRequestsForwardImmediately(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func() *http.Request
	}{{
		name: "POST (a mutation is never coalesced)",
		req: func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/repos/o/r/statuses/abc", strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer tok")
			return r
		},
	}, {
		name: "conditional If-None-Match",
		req: func() *http.Request {
			r := debounceReq("/repos/o/r/x", "tok")
			r.Header.Set("If-None-Match", `W/"deadbeef"`)
			return r
		},
	}, {
		name: "conditional If-Modified-Since",
		req: func() *http.Request {
			r := debounceReq("/repos/o/r/x", "tok")
			r.Header.Set("If-Modified-Since", "Wed, 21 Oct 2026 07:28:00 GMT")
			return r
		},
	}, {
		name: "Range read",
		req: func() *http.Request {
			r := debounceReq("/repos/o/r/x", "tok")
			r.Header.Set("Range", "bytes=0-99")
			return r
		},
	}, {
		name: "no bearer token",
		req:  func() *http.Request { return httptest.NewRequest(http.MethodGet, "/repos/o/r/x", nil) },
	}} {
		t.Run(tc.name, func(t *testing.T) {
			u := &countingUpstream{}
			// A window far longer than the assertion below tolerates.
			d := NewDebouncer(2 * time.Second)
			t.Cleanup(func() { d.Drain(2 * time.Second) })
			h := d.Wrap(u.handler())

			start := time.Now()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tc.req())
			elapsed := time.Since(start)

			assert.Less(t, elapsed, 500*time.Millisecond, "an ineligible request must not be held")
			assert.Equal(t, 1, u.count(), "it reaches upstream on its own")
			assert.Empty(t, w.Header().Get(debounceHeader), "and is not marked as debounced")
		})
	}
}

// TestDebounce_HoldsForTheFullWindow: the delay is deliberate. Even a lone
// request waits — that is the "discourage the uncacheable endpoints" half of
// the feature, and the reported hold reflects real elapsed time.
func TestDebounce_HoldsForTheFullWindow(t *testing.T) {
	u := &countingUpstream{}
	const window = 250 * time.Millisecond
	d := NewDebouncer(window)
	t.Cleanup(func() { d.Drain(2 * time.Second) })
	h := d.Wrap(u.handler())

	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, debounceReq("/repos/o/r/actions/runs?status=queued", "tok"))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, window, "a single request still pays the window")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "1", w.Header().Get(debounceBatchHeader), "a batch of one saved nothing, honestly reported")
}

// TestDebounce_ArrivalAfterTheFetchStartsGetsAFreshBatch: a batch's call
// is issued, it is closed. A request arriving later must never be handed an
// answer that was already in flight before it asked.
func TestDebounce_ArrivalAfterTheFetchStartsGetsAFreshBatch(t *testing.T) {
	u := &countingUpstream{delay: 150 * time.Millisecond}
	d := NewDebouncer(50 * time.Millisecond)
	t.Cleanup(func() { d.Drain(3 * time.Second) })
	h := d.Wrap(u.handler())

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, debounceReq("/repos/o/r/x", "tok"))
		first <- w
	}()

	// Arrive while the batch is mid-fetch (window closed, call issued).
	time.Sleep(120 * time.Millisecond)
	late := httptest.NewRecorder()
	h.ServeHTTP(late, debounceReq("/repos/o/r/x", "tok"))

	<-first
	assert.Equal(t, 2, u.count(), "the late arrival opens its own batch rather than reusing a closed one")
	assert.Equal(t, `{"call":2}`, late.Body.String())
}

// TestDebounce_WaiterCancellationDoesNotKillTheBatch: the batch fetch runs on a
// context detached from every waiter (the freshness-fetch doctrine), so
// caller hanging up must not cost the others their answer.
func TestDebounce_WaiterCancellationDoesNotKillTheBatch(t *testing.T) {
	u := &countingUpstream{delay: 100 * time.Millisecond}
	d := NewDebouncer(60 * time.Millisecond)
	t.Cleanup(func() { d.Drain(3 * time.Second) })
	h := d.Wrap(u.handler())

	// The arrival, whose request is cloned for the batch, disconnects almost immediately.
	gone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(gone)
		h.ServeHTTP(httptest.NewRecorder(), debounceReq("/repos/o/r/x", "tok").WithContext(ctx))
	}()
	time.Sleep(10 * time.Millisecond)

	stayed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, debounceReq("/repos/o/r/x", "tok"))
		stayed <- w
	}()
	time.Sleep(10 * time.Millisecond)
	cancel() // the batch's originating caller goes away
	<-gone

	w := <-stayed
	assert.Equal(t, http.StatusOK, w.Code, "the remaining waiter is still answered")
	assert.Equal(t, `{"call":1}`, w.Body.String())
	assert.Equal(t, 1, u.count())
}

// TestDebounce_OversizedResponseFallsBackPerWaiter: a body too large to hold
// in memory cannot be fanned out, so the batch is abandoned and each waiter
// pays for its own call — never a truncated shared answer.
func TestDebounce_OversizedResponseFallsBackPerWaiter(t *testing.T) {
	big := strings.Repeat("x", debounceMaxBodyBytes+1024)
	u := &countingUpstream{body: func(*http.Request) string { return big }}
	d := NewDebouncer(60 * time.Millisecond)
	t.Cleanup(func() { d.Drain(5 * time.Second) })
	h := d.Wrap(u.handler())

	recs := fireConcurrent(h, 3, func(int) *http.Request { return debounceReq("/repos/o/r/big", "tok") })

	//  batch attempt (discarded) plus direct call per waiter.
	assert.Equal(t, 4, u.count(), "waiters refetch individually rather than share a truncated body")
	for i, w := range recs {
		assert.Equal(t, http.StatusOK, w.Code, "waiter %d", i)
		assert.Equal(t, len(big), w.Body.Len(), "waiter %d got the WHOLE body", i)
	}
}

// TestDebounce_SharesNon200Answers: an error is a real answer to an identical
// question. Coalescing must not quietly convert a shared into N calls.
func TestDebounce_SharesNon200Answers(t *testing.T) {
	u := &countingUpstream{status: http.StatusNotFound, body: func(*http.Request) string {
		return `{"message":"Not Found"}`
	}}
	d := NewDebouncer(60 * time.Millisecond)
	t.Cleanup(func() { d.Drain(2 * time.Second) })
	h := d.Wrap(u.handler())

	recs := fireConcurrent(h, 3, func(int) *http.Request { return debounceReq("/repos/o/nope/x", "tok") })
	assert.Equal(t, 1, u.count())
	for i, w := range recs {
		assert.Equal(t, http.StatusNotFound, w.Code, "waiter %d keeps the upstream status", i)
		assert.Equal(t, `{"message":"Not Found"}`, w.Body.String())
	}
}

// TestDebounce_DrainCutsTheWindowShort: at shutdown the waiters are still on
// the wire, so Drain must answer them NOW rather than making shutdown sit out
// the full hold.
func TestDebounce_DrainCutsTheWindowShort(t *testing.T) {
	u := &countingUpstream{}
	d := NewDebouncer(10 * time.Second) // never elapses within this test
	h := d.Wrap(u.handler())

	served := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, debounceReq("/repos/o/r/x", "tok"))
		served <- w
	}()
	time.Sleep(50 * time.Millisecond) // let it join and start holding

	start := time.Now()
	d.Drain(5 * time.Second)
	assert.Less(t, time.Since(start), 3*time.Second, "Drain cuts the pending window instead of waiting it out")

	select {
	case w := <-served:
		assert.Equal(t, http.StatusOK, w.Code, "the held request is answered, not dropped")
		assert.Equal(t, 1, u.count())
	case <-time.After(2 * time.Second):
		t.Fatal("the held request was never answered")
	}

	// After draining, later requests forward directly rather than hanging.
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, debounceReq("/repos/o/r/x", "tok")) }()
	select {
	case <-done:
		assert.Equal(t, http.StatusOK, w.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("a post-drain request must forward immediately, not queue")
	}
}

// TestDebounce_NilIsInert: a window of disables the feature outright, and the
// nil Debouncer that represents it must be safe on every method.
func TestDebounce_NilIsInert(t *testing.T) {
	var d *Debouncer
	require.Nil(t, NewDebouncer(0), "a zero window disables coalescing")
	require.Nil(t, NewDebouncer(-time.Second))

	u := &countingUpstream{}
	h := d.Wrap(u.handler())
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, debounceReq("/repos/o/r/x", "tok"))

	assert.Less(t, time.Since(start), 500*time.Millisecond)
	assert.Equal(t, 1, u.count())
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(debounceHeader))
	assert.Zero(t, d.Window())
	d.attach(nil, nil) // must not panic
	d.Drain(time.Second)
}

// TestDebounce_ThroughRouterRecordsStats: see docs/testing/test-harness.md.
func TestDebounce_ThroughRouterRecordsStats(t *testing.T) {
	svc := configuredAuth(t)
	u := newWorkflowRunsUpstream()
	// Window must outlast the arrival spread, or the batch splits; see docs/testing/test-harness.md.
	s := newFullTestStackDebounced(t, svc, u.handler(), 500*time.Millisecond)

	const n = 4
	target := "/repos/org1/repo1/actions/runs?event=push&per_page=100"
	// A different route shape, invisible to the assertions below.
	warm := do(t, s.router, authedReq("GET", "/repos/org1/repo1", nil))
	require.Equal(t, http.StatusOK, warm.Code, "the warm-up must actually resolve the caller")

	recs := fireConcurrent(s.router, n, func(int) *http.Request { return authedReq("GET", target, nil) })
	for i, w := range recs {
		require.Equal(t, http.StatusOK, w.Code, "waiter %d", i)
		assert.NotEmpty(t, w.Header().Get(debounceHeader), "waiter %d was debounced", i)
	}

	g := groupFor(t, s.router, svc, "GET /repos/{owner}/{repo}/actions/runs")
	assert.EqualValues(t, n, g.Passthrough, "every waiter is still its own recorded request")
	assert.EqualValues(t, n, g.ByReason[PassQuery], "and keeps its uncacheable-shape reason")
	assert.EqualValues(t, n, g.Debounced, "all four were held")
	assert.EqualValues(t, n-1, g.UpstreamSaved, "one call served four requests: three never happened")
}
