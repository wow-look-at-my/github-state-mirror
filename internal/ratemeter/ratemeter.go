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

type key struct{ identity, resource string }

// Store holds the latest observation per (identity, resource). All methods
// are safe for concurrent use and no-op on a nil *Store (the nil-recorder
// pattern), so wiring may pass a nil meter without guards.
type Store struct {
	mu  sync.Mutex
	obs map[key]Observation
}

// New returns an empty Store.
func New() *Store { return &Store{obs: make(map[key]Observation)} }

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
	s.mu.Lock()
	defer s.mu.Unlock()
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
		ObservedAt: time.Now(),
	}
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

// Snapshot returns every observation, sorted by identity then resource.
func (s *Store) Snapshot() []Observation {
	if s == nil {
		return nil
	}
	s.mu.Lock()
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
