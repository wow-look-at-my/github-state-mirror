// Package latency keeps in-memory latency histograms per operation kind, for
// the admin dashboard. It is a live view like the request log: it resets on
// restart. It is stdlib-only so any package can import it.
package latency

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Bounds are the bucket upper edges. The last bucket takes everything above
// the last edge, so no observation is dropped.
var Bounds = []time.Duration{
	100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond,
	10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond,
	100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2500 * time.Millisecond, 5 * time.Second,
	10 * time.Second, 30 * time.Second,
}

// Recorder is safe for concurrent use. A nil Recorder records nothing.
type Recorder struct {
	mu    sync.Mutex
	since time.Time
	kinds map[string]*hist
	// onObserve: see SetOnObserve.
	onObserve func(kind string)
}

// SetOnObserve installs a hook that runs after every observation, outside the
// lock, with its kind. Call it before the first observation.
func (r *Recorder) SetOnObserve(fn func(kind string)) {
	if r != nil {
		r.onObserve = fn
	}
}

type hist struct {
	counts []int64
	n      int64
	total  time.Duration
	max    time.Duration
}

func New() *Recorder {
	return &Recorder{since: time.Now(), kinds: map[string]*hist{}}
}

// Observe records one duration under kind.
func (r *Recorder) Observe(kind string, d time.Duration) {
	if r == nil {
		return
	}
	i := sort.Search(len(Bounds), func(i int) bool { return d <= Bounds[i] })
	r.mu.Lock()
	h := r.kinds[kind]
	if h == nil {
		h = &hist{counts: make([]int64, len(Bounds)+1)}
		r.kinds[kind] = h
	}
	h.counts[i]++
	h.n++
	h.total += d
	if d > h.max {
		h.max = d
	}
	r.mu.Unlock()
	if r.onObserve != nil {
		r.onObserve(kind)
	}
}

// Kind is one operation kind's distribution as the API serves it.
type Kind struct {
	Kind   string  `json:"kind"`
	Count  int64   `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
	// Buckets[i] counts observations <= BoundsMs[i]; the extra last one counts the rest.
	Buckets []int64 `json:"buckets"`
}

type Snapshot struct {
	Since    time.Time `json:"since"`
	BoundsMs []float64 `json:"bounds_ms"`
	Kinds    []Kind    `json:"kinds"`
}

// Snapshot copies the histograms out, sorted by kind. A percentile is the
// upper edge of the bucket that holds it, capped at the observed max.
func (r *Recorder) Snapshot() Snapshot {
	out := Snapshot{BoundsMs: make([]float64, len(Bounds)), Kinds: []Kind{}}
	for i, b := range Bounds {
		out.BoundsMs[i] = ms(b)
	}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out.Since = r.since
	for name, h := range r.kinds {
		k := Kind{Kind: name, Count: h.n, MaxMs: ms(h.max), Buckets: append([]int64(nil), h.counts...)}
		if h.n > 0 {
			k.MeanMs = ms(h.total) / float64(h.n)
		}
		k.P50Ms = h.quantile(0.50)
		k.P95Ms = h.quantile(0.95)
		k.P99Ms = h.quantile(0.99)
		out.Kinds = append(out.Kinds, k)
	}
	sort.Slice(out.Kinds, func(i, j int) bool { return out.Kinds[i].Kind < out.Kinds[j].Kind })
	return out
}

func (h *hist) quantile(q float64) float64 {
	if h.n == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(h.n)))
	var seen int64
	for i, c := range h.counts {
		seen += c
		if seen < rank {
			continue
		}
		if i == len(Bounds) || Bounds[i] > h.max {
			return ms(h.max)
		}
		return ms(Bounds[i])
	}
	return ms(h.max)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
