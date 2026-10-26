// writeGate admits one of this process's writers to a database file at a time, in arrival order.

package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// writeGate admits one of this process's writers to a database file at a
// time. Foreground writers go first, in arrival order.
type writeGate struct {
	mu   sync.Mutex
	held bool
	// queue holds one waiter per waiting foreground writer, oldest first; release closes the head's turn.
	queue []*gateWaiter
	// background holds the waiting background writers, oldest first.
	background []*gateWaiter
	// holder is the admitted writer's goroutine id while holder tracking is on.
	holder    atomic.Int64
	heldSince atomic.Int64
	// obs receives the wait for the gate and the time it was held; nil times nothing.
	obs Observer
}

// Kinds the gate reports: a foreground writer's wait for the write lock, a
// background writer's wait, and how long each writer kept it.
const (
	KindWriteWait           = "db.write_wait"
	KindWriteWaitBackground = "db.write_wait.background"
	KindWriteHold           = "db.write_hold"
)

// slowHold is the hold past which the releasing writer's stack is logged.
const slowHold = time.Second

// writeGateWait matches busy_timeout: a writer that cannot get in fails after the same wait it had.
const writeGateWait = 30 * time.Second

// ErrWriteGateTimeout is returned by a write that waited writeGateWait for the write lock without getting it.
var ErrWriteGateTimeout = errors.New("database is locked: waited " + writeGateWait.String() + " for the write lock")

// backgroundMaxWait bounds how long foreground writers may keep passing a waiting background writer.
const backgroundMaxWait = 500 * time.Millisecond

// gateWaiter is one queued writer: its turn closes when the gate is handed to it.
type gateWaiter struct {
	turn  chan struct{}
	since time.Time
}

type backgroundKey struct{}

// WithBackgroundWrites marks every write made with ctx as background work.
func WithBackgroundWrites(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackgroundWrite reports whether ctx was marked by WithBackgroundWrites.
func IsBackgroundWrite(ctx context.Context) bool {
	bg, _ := ctx.Value(backgroundKey{}).(bool)
	return bg
}

func newWriteGate(obs Observer) *writeGate { return &writeGate{obs: obs} }

func (g *writeGate) acquire(ctx context.Context) error {
	start := time.Now()
	g.mu.Lock()
	if !g.held {
		g.held = true
		g.mu.Unlock()
		g.admitted(start, IsBackgroundWrite(ctx))
		return nil
	}
	w := &gateWaiter{turn: make(chan struct{}), since: start}
	if IsBackgroundWrite(ctx) {
		g.background = append(g.background, w)
	} else {
		g.queue = append(g.queue, w)
	}
	g.mu.Unlock()

	timer := time.NewTimer(writeGateWait)
	defer timer.Stop()
	select {
	case <-w.turn:
		g.admitted(start, IsBackgroundWrite(ctx))
		return nil
	case <-ctx.Done():
		return g.abandon(w, ctx.Err())
	case <-timer.C:
		return g.abandon(w, ErrWriteGateTimeout)
	}
}

// abandon leaves the queue. Consider a turn handed over while the waiter.
// That turn was giving up is passed on to the next writer, so the gate is
// never left held by nobody.
func (g *writeGate) abandon(w *gateWaiter, err error) error {
	g.mu.Lock()
	if removeWaiter(&g.queue, w) || removeWaiter(&g.background, w) {
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()
	g.release()
	return err
}

func removeWaiter(list *[]*gateWaiter, w *gateWaiter) bool {
	for i, x := range *list {
		if x == w {
			*list = append((*list)[:i], (*list)[i+1:]...)
			return true
		}
	}
	return false
}

// next pops the writer the gate goes to: the oldest foreground writer, unless
// none waits or the oldest background writer has waited backgroundMaxWait.
// Callers hold g.mu.
func (g *writeGate) next(now time.Time) *gateWaiter {
	bgDue := len(g.background) > 0 && (len(g.queue) == 0 || now.Sub(g.background[0].since) >= backgroundMaxWait)
	if bgDue {
		w := g.background[0]
		g.background = g.background[1:]
		return w
	}
	if len(g.queue) == 0 {
		return nil
	}
	w := g.queue[0]
	g.queue = g.queue[1:]
	return w
}

func (g *writeGate) admitted(start time.Time, background bool) {
	now := time.Now()
	g.heldSince.Store(now.UnixNano())
	if g.obs != nil {
		kind := KindWriteWait
		if background {
			kind = KindWriteWaitBackground
		}
		g.obs.Observe(kind, now.Sub(start))
	}
	if trackHolders.Load() {
		id := goroutineID()
		g.holder.Store(id)
		holders.add(id)
	}
}

// release hands the gate straight to the next waiter, or frees it when nobody waits.
func (g *writeGate) release() {
	if since := g.heldSince.Swap(0); since != 0 {
		g.recordHold(time.Duration(time.Now().UnixNano() - since))
	}
	if id := g.holder.Swap(0); id != 0 {
		holders.remove(id)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.next(time.Now())
	if w == nil {
		g.held = false
		return
	}
	close(w.turn)
}

// recordHold times one writer's hold, and names a slow holder by the stack that released it.
func (g *writeGate) recordHold(d time.Duration) {
	if g.obs != nil {
		g.obs.Observe(KindWriteHold, d)
	}
	if d < slowHold {
		return
	}
	stack := make([]byte, 8<<10)
	stack = stack[:runtime.Stack(stack, false)]
	slog.Warn("database: write lock held past the slow-hold threshold", "held", d, "threshold", slowHold, "queued", g.queued(), "holder", string(stack))
}

// queued reports how many writers wait for the gate.
func (g *writeGate) queued() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queue) + len(g.background)
}

// isWriteSQL reports whether a statement run through QueryContext writes (an
// INSERT/UPDATE/DELETE/REPLACE, typically with RETURNING). Leading comments,
// which sqlc emits, are skipped.
func isWriteSQL(query string) bool {
	q := skipLeadingComments(query)
	word := q
	if i := strings.IndexAny(q, " \t\r\n("); i >= 0 {
		word = q[:i]
	}
	switch strings.ToUpper(word) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE":
		return true
	case "WITH":
		upper := strings.ToUpper(q)
		for _, verb := range []string{"INSERT ", "UPDATE ", "DELETE ", "REPLACE "} {
			if strings.Contains(upper, verb) {
				return true
			}
		}
	}
	return false
}

func skipLeadingComments(q string) string {
	for {
		q = strings.TrimLeft(q, " \t\r\n")
		switch {
		case strings.HasPrefix(q, "--"):
			i := strings.IndexByte(q, '\n')
			if i < 0 {
				return ""
			}
			q = q[i+1:]
		case strings.HasPrefix(q, "/*"):
			i := strings.Index(q, "*/")
			if i < 0 {
				return ""
			}
			q = q[i+2:]
		default:
			return q
		}
	}
}

// ---- holder tracking: which goroutine holds a write lock ----

// trackHolders turns on per-goroutine holder bookkeeping.
var trackHolders atomic.Bool

var holders = holderSet{n: map[int64]int{}}

type holderSet struct {
	mu sync.Mutex
	n  map[int64]int
}

func (h *holderSet) add(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[id]++
}

func (h *holderSet) remove(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n[id]--; h.n[id] <= 0 {
		delete(h.n, id)
	}
}

func (h *holderSet) has(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[id] > 0
}

// TrackWriteHolders records which goroutine holds each write lock from now on, so CallerHoldsWrite can answer.
func TrackWriteHolders() { trackHolders.Store(true) }

// CallerHoldsWrite reports whether the calling goroutine holds a database
// write lock: an open transaction, or a write statement in progress.
func CallerHoldsWrite() bool {
	if !trackHolders.Load() {
		panic("database: CallerHoldsWrite needs TrackWriteHolders")
	}
	return holders.has(goroutineID())
}

// goroutineID parses the current goroutine's id out of its stack header,
// "goroutine 123 [running]:".
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s, ok := bytes.CutPrefix(buf[:n], []byte("goroutine "))
	if !ok {
		panic("database: unexpected stack header " + strconv.Quote(string(buf[:n])))
	}
	end := bytes.IndexByte(s, ' ')
	if end < 0 {
		panic("database: unexpected stack header " + strconv.Quote(string(buf[:n])))
	}
	id, err := strconv.ParseInt(string(s[:end]), 10, 64)
	if err != nil {
		panic("database: unexpected stack header " + strconv.Quote(string(buf[:n])) + ": " + err.Error())
	}
	return id
}

// ---- the network-inside-a-transaction check ----

// ErrRequestUnderWriteLock is returned for a request sent by a goroutine that holds a database write lock.
var ErrRequestUnderWriteLock = errors.New("outbound request sent while holding the database write lock")

var violations struct {
	mu   sync.Mutex
	list []string
}

// NoWriteHeldTransport wraps base so that a request sent by a goroutine
// holding a database write lock fails with ErrRequestUnderWriteLock.
func NoWriteHeldTransport(base http.RoundTripper) http.RoundTripper {
	TrackWriteHolders()
	return noWriteHeldTransport{base: base}
}

type noWriteHeldTransport struct{ base http.RoundTripper }

func (t noWriteHeldTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if CallerHoldsWrite() {
		stack := make([]byte, 16<<10)
		stack = stack[:runtime.Stack(stack, false)]
		violations.mu.Lock()
		violations.list = append(violations.list, fmt.Sprintf("%s %s\n%s", req.Method, req.URL, stack))
		violations.mu.Unlock()
		return nil, fmt.Errorf("%w: %s %s", ErrRequestUnderWriteLock, req.Method, req.URL)
	}
	return t.base.RoundTrip(req)
}

// WriteHeldRequests returns every request NoWriteHeldTransport refused, each
// with the sending goroutine's stack.
func WriteHeldRequests() []string {
	violations.mu.Lock()
	defer violations.mu.Unlock()
	return append([]string(nil), violations.list...)
}

// RunWithTxGuard runs a test binary with http.DefaultTransport wrapped in
// NoWriteHeldTransport. RunWithTxGuard fails it if any test sent a request
// while holding a database write lock. Every outbound client here bottoms out
// in http.DefaultTransport. Call it from TestMain before anything builds a
// client.
func RunWithTxGuard(m interface{ Run() int }) int {
	http.DefaultTransport = NoWriteHeldTransport(http.DefaultTransport)
	code := m.Run()
	held := WriteHeldRequests()
	for _, v := range held {
		fmt.Fprintf(os.Stderr, "FAIL: request sent while holding the database write lock: %s\n", v)
	}
	if len(held) > 0 && code == 0 {
		return 1
	}
	return code
}
