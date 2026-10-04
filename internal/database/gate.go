package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// writeGate admits one of this process's writers to a database file at a
// time, in arrival order.
type writeGate struct {
	// slot holds one token while a writer is admitted.
	slot chan struct{}
	// holder is the admitted writer's goroutine id while holder tracking is on.
	holder atomic.Int64
}

// writeGateWait matches busy_timeout: a writer that cannot get in fails after the same wait it had.
const writeGateWait = 30 * time.Second

// ErrWriteGateTimeout is returned by a write that waited writeGateWait for the write lock without getting it.
var ErrWriteGateTimeout = errors.New("database is locked: waited " + writeGateWait.String() + " for the write lock")

func newWriteGate() *writeGate { return &writeGate{slot: make(chan struct{}, 1)} }

func (g *writeGate) acquire(ctx context.Context) error {
	select {
	case g.slot <- struct{}{}:
	default:
		timer := time.NewTimer(writeGateWait)
		defer timer.Stop()
		select {
		case g.slot <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrWriteGateTimeout
		}
	}
	if trackHolders.Load() {
		id := goroutineID()
		g.holder.Store(id)
		holders.add(id)
	}
	return nil
}

func (g *writeGate) release() {
	if id := g.holder.Swap(0); id != 0 {
		holders.remove(id)
	}
	<-g.slot
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
