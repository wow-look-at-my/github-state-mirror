// Package ratemeter passively tracks GitHub's X-RateLimit-* response headers, in memory, for free.
// see docs/ratemeter.md
package ratemeter

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Observation is the most recent rate-limit reading for one (identity,
// resource) pair, parsed off a response's X-RateLimit-* headers.
type Observation struct {
	// Identity labels WHO was consuming the limit; never a raw token value. see docs/ratemeter.md
	Identity string
	// Name is the identity's verified display name, display-only, never part of the key. see docs/ratemeter.md
	Name string
	// Resource is the GitHub rate-limit bucket ("core" when the header is absent).
	Resource  string
	Limit     int
	Remaining int
	Used      int
	// Reset is when the window resets (X-RateLimit-Reset, Unix epoch seconds).
	Reset int64
	// ObservedAt is when the response carrying this reading was seen.
	ObservedAt time.Time
}

// maxEntries bounds the map; on overflow the entry with the oldest ObservedAt is evicted. see docs/ratemeter.md
const maxEntries = 512

// staleTTL bounds an observation with no usable Reset. see docs/ratemeter.md
const staleTTL = time.Hour

type key struct{ identity, resource string }

// Store holds the latest observation per (identity, resource). All methods
type Store struct {
	mu  sync.Mutex
	obs map[key]Observation
	// now is the clock, a test seam set once at construction; tests override it before use.
	now func() time.Time
}

// New returns an empty Store.
func New() *Store { return &Store{obs: make(map[key]Observation), now: time.Now} }

// Observe parses the X-RateLimit-* headers off resp and records the reading
// under identity. name is the identity's verified display name ("" when
// unknown; a named entry keeps its last known name across nameless
// observations of the same identity). A response carrying neither
// X-RateLimit-Limit nor a usable X-RateLimit-Remaining (304s, non-API hosts,
// ...) is ignored — a partial reading is garbage, so both must parse.
// X-RateLimit-Used is derived as limit-remaining when absent. Last write
// wins: Observe runs at response time, so the latest call is the freshest
// reading.
func (s *Store) Observe(identity, name string, resp *http.Response) {
	if s == nil || resp == nil {
		return
	}
	limit, okLimit := atoi(resp.Header.Get("X-RateLimit-Limit"))
	remaining, okRemaining := atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if !okLimit || !okRemaining {
		return
	}
	used, ok := atoi(resp.Header.Get("X-RateLimit-Used"))
	if !ok {
		used = max(limit-remaining, 0)
	}
	var reset int64
	if v, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		reset = v
	}
	resource := resp.Header.Get("X-RateLimit-Resource")
	if resource == "" {
		resource = "core"
	}
	if identity == "" {
		identity = "anonymous"
	}

	k := key{identity: identity, resource: resource}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	prev, exists := s.obs[k]
	if !exists && len(s.obs) >= maxEntries {
		s.evictOldestLocked()
	}
	if name == "" {
		// Keep the last verified name: a nameless reading is still the same actor.
		name = prev.Name
	}
	s.obs[k] = Observation{
		Identity:   identity,
		Name:       name,
		Resource:   resource,
		Limit:      limit,
		Remaining:  remaining,
		Used:       used,
		Reset:      reset,
		ObservedAt: now,
	}
}

// pruneLocked drops dead observations; called with s.mu held on every write and read (lazy expiry).
func (s *Store) pruneLocked(now time.Time) {
	for k, o := range s.obs {
		if dead(o, now) {
			delete(s.obs, k)
		}
	}
}

// dead reports whether an observation has outlived its meaning. see docs/ratemeter.md
func dead(o Observation, now time.Time) bool {
	if o.Reset > 0 {
		return now.Unix() > o.Reset
	}
	return now.Sub(o.ObservedAt) > staleTTL
}

// evictOldestLocked drops the entry with the oldest ObservedAt. Called with
// s.mu held, only when the map is at capacity (rare), so a linear scan is fine.
func (s *Store) evictOldestLocked() {
	var oldest key
	var oldestAt time.Time
	found := false
	for k, o := range s.obs {
		if !found || o.ObservedAt.Before(oldestAt) {
			oldest, oldestAt, found = k, o.ObservedAt, true
		}
	}
	if found {
		delete(s.obs, oldest)
	}
}

// Snapshot returns every live observation, sorted by identity then resource.
// Dead entries (see dead) are pruned first, so a snapshot never carries an
// observation whose window already rolled over.
func (s *Store) Snapshot() []Observation {
	if s == nil {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	s.pruneLocked(now)
	out := make([]Observation, 0, len(s.obs))
	for _, o := range s.obs {
		out = append(out, o)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity != out[j].Identity {
			return out[i].Identity < out[j].Identity
		}
		return out[i].Resource < out[j].Resource
	})
	return out
}

// ObservationsFor returns the live observations for one identity, across every resource, sorted by resource.
// see docs/ratemeter.md
func (s *Store) ObservationsFor(identity string) []Observation {
	if s == nil {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	s.pruneLocked(now)
	var out []Observation
	for k, o := range s.obs {
		if k.identity == identity {
			out = append(out, o)
		}
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

// atoi parses a non-negative header value, reporting whether it was present
// and numeric.
func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
