// Package ratemeter passively tracks GitHub's X-RateLimit-* response headers.
//
// GitHub attaches rate-limit headers to every API response, so the mirror can
// know each credential's standing for free — no GET /rate_limit polling spend.
// Every upstream path (the passthrough proxy, cached-route miss fetches, the
// reveal probe, and ghclient's own calls) reports its responses here, and the
// admin dashboard's "Rate limit" tab reads the snapshot.
//
// The store is deliberately IN-MEMORY — the same live-view-not-audit-log
// stance as the api package's request log: rate-limit windows reset within the
// hour, so the data is sub-hour-ephemeral, and persisting it would add a
// schema.sql table whose SchemaVersion bump nukes the whole cache. It resets
// on restart.
//
// Dead observations age out. An actively observed identity is re-observed
// with a fresh future reset on every request, so an entry whose reset moment
// has passed belongs to an identity that stopped calling — it is pruned. An
// entry with no usable reset (a zero Reset) has no window to judge by and is
// instead pruned once its ObservedAt is older than staleTTL. Pruning is lazy
// — swept on Observe (write) and Snapshot (read), the kv package's
// lazy-expiry stance — with no background goroutine or timer; the maxEntries
// cap stays as the size backstop.
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
	// Identity labels WHO was consuming the limit: the request's principal
	// ("user:<id>", "app:<id>", "app-installation:<id>", a token fingerprint)
	// or a credential-derived label ("app-jwt", "token:<fp12>"). Never a raw
	// token value.
	Identity string
	// Resource is the GitHub rate-limit bucket the reading is for
	// (X-RateLimit-Resource: "core", "graphql", "search", ...). "core" when
	// the header is absent.
	Resource  string
	Limit     int
	Remaining int
	Used      int
	// Reset is when the window resets (X-RateLimit-Reset, Unix epoch seconds).
	Reset int64
	// ObservedAt is when the response carrying this reading was seen.
	ObservedAt time.Time
}

// maxEntries bounds the map: unbounded distinct identities (e.g. rotating
// token fingerprints) must not grow it forever. On overflow the entry with
// the oldest ObservedAt is evicted.
const maxEntries = 512

// staleTTL bounds observations whose Reset is unknown (zero — header absent
// or unparseable): with no reset moment to judge deadness by, an entry
// unseen for longer than the longest standard rate window (one hour) is
// dead. A zero reset must neither mean immortal nor mean instant death.
const staleTTL = time.Hour

type key struct{ identity, resource string }

// Store holds the latest observation per (identity, resource). All methods
// are safe for concurrent use and no-op on a nil *Store (the nil-recorder
// pattern), so wiring may pass a nil meter without guards.
type Store struct {
	mu  sync.Mutex
	obs map[key]Observation
	// now is the clock, a test seam. Set once at construction (time.Now);
	// tests override it before use.
	now func() time.Time
}

// New returns an empty Store.
func New() *Store { return &Store{obs: make(map[key]Observation), now: time.Now} }

// Observe parses the X-RateLimit-* headers off resp and records the reading
// under identity. A response carrying neither X-RateLimit-Limit nor a usable
// X-RateLimit-Remaining (304s, non-API hosts, ...) is ignored — a partial
// reading is garbage, so both must parse. X-RateLimit-Used is derived as
// limit-remaining when absent. Last write wins: Observe runs at response
// time, so the latest call is the freshest reading.
func (s *Store) Observe(identity string, resp *http.Response) {
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
	if _, exists := s.obs[k]; !exists && len(s.obs) >= maxEntries {
		s.evictOldestLocked()
	}
	s.obs[k] = Observation{
		Identity:   identity,
		Resource:   resource,
		Limit:      limit,
		Remaining:  remaining,
		Used:       used,
		Reset:      reset,
		ObservedAt: now,
	}
}

// pruneLocked drops dead observations (see dead). Called with s.mu held on
// every write (Observe) and read (Snapshot) — lazy expiry, no background
// sweeper. The map is bounded by maxEntries, so the linear sweep is cheap.
func (s *Store) pruneLocked(now time.Time) {
	for k, o := range s.obs {
		if dead(o, now) {
			delete(s.obs, k)
		}
	}
}

// dead reports whether an observation has outlived its meaning. An actively
// observed identity is re-observed with a fresh future reset on every
// request; only an identity unseen since its window rolled over keeps a past
// reset — so a reset strictly in the past marks the entry dead. A zero (or
// negative — garbage) Reset has no window to judge by and instead dies once
// ObservedAt is more than staleTTL old.
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
