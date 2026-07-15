package ratemeter

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// respWith builds a response carrying the given headers.
func respWith(headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: h}
}

// pinned fixes the store's clock (the now seam) at a settable instant and
// returns the setter.
func pinned(s *Store, at time.Time) func(time.Time) {
	current := at
	s.now = func() time.Time { return current }
	return func(t time.Time) { current = t }
}

func TestObserve_ParsesHeaders(t *testing.T) {
	s := New()
	// Pin the clock before the fixture's reset so the entry is live.
	pinned(s, time.Unix(1767225000, 0))
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4321",
		"X-RateLimit-Used":      "679",
		"X-RateLimit-Reset":     "1767225600",
		"X-RateLimit-Resource":  "graphql",
	}))

	snap := s.Snapshot()
	require.Len(t, snap, 1)
	o := snap[0]
	assert.Equal(t, "user:42", o.Identity)
	assert.Equal(t, "graphql", o.Resource)
	assert.Equal(t, 5000, o.Limit)
	assert.Equal(t, 4321, o.Remaining)
	assert.Equal(t, 679, o.Used)
	assert.Equal(t, int64(1767225600), o.Reset)
	assert.False(t, o.ObservedAt.IsZero())
}

// TestObserve_ResourceDefaultsToCore: without X-RateLimit-Resource the reading
// belongs to the default "core" bucket.
func TestObserve_ResourceDefaultsToCore(t *testing.T) {
	s := New()
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4999",
	}))

	snap := s.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "core", snap[0].Resource)
}

// TestObserve_UsedDerivedWhenAbsent: X-RateLimit-Used missing -> limit-remaining.
func TestObserve_UsedDerivedWhenAbsent(t *testing.T) {
	s := New()
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
	}))

	snap := s.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, 1000, snap[0].Used)
}

// TestObserve_IgnoresResponsesWithoutRateHeaders: 304s and non-API hosts carry
// no X-RateLimit-* headers; nothing is recorded, and a partial reading (only
// one of Limit/Remaining) is discarded too.
func TestObserve_IgnoresResponsesWithoutRateHeaders(t *testing.T) {
	s := New()
	s.Observe("user:42", respWith(nil))
	s.Observe("user:42", respWith(map[string]string{"X-RateLimit-Limit": "5000"}))
	s.Observe("user:42", respWith(map[string]string{"X-RateLimit-Remaining": "10"}))
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "junk",
		"X-RateLimit-Remaining": "10",
	}))
	assert.Empty(t, s.Snapshot())
}

// TestObserve_LastWriteWins: a later reading for the same (identity, resource)
// replaces the earlier one.
func TestObserve_LastWriteWins(t *testing.T) {
	s := New()
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
	}))
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "3999",
	}))

	snap := s.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, 3999, snap[0].Remaining)
}

// TestObserve_EmptyIdentityLabeled: an empty identity is recorded as
// "anonymous" rather than an invisible blank row.
func TestObserve_EmptyIdentityLabeled(t *testing.T) {
	s := New()
	s.Observe("", respWith(map[string]string{
		"X-RateLimit-Limit":     "60",
		"X-RateLimit-Remaining": "59",
	}))

	snap := s.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "anonymous", snap[0].Identity)
}

// TestObserve_Bounded: distinct identities beyond maxEntries evict older
// entries instead of growing the map forever, and the newest entry survives.
func TestObserve_Bounded(t *testing.T) {
	s := New()
	for i := 0; i < maxEntries+50; i++ {
		s.Observe(fmt.Sprintf("token:%012d", i), respWith(map[string]string{
			"X-RateLimit-Limit":     "5000",
			"X-RateLimit-Remaining": "4000",
		}))
	}
	snap := s.Snapshot()
	assert.LessOrEqual(t, len(snap), maxEntries)

	last := fmt.Sprintf("token:%012d", maxEntries+49)
	found := false
	for _, o := range snap {
		if o.Identity == last {
			found = true
		}
	}
	assert.True(t, found, "the most recent observation must survive eviction")
}

// TestSnapshot_Sorted: identity then resource ordering, so the dashboard's
// group-by-identity rendering gets adjacent rows.
func TestSnapshot_Sorted(t *testing.T) {
	s := New()
	for _, in := range []struct{ id, res string }{
		{"user:9", "search"}, {"app:1", "graphql"}, {"user:9", "core"}, {"app:1", "core"},
	} {
		s.Observe(in.id, respWith(map[string]string{
			"X-RateLimit-Limit":     "10",
			"X-RateLimit-Remaining": "9",
			"X-RateLimit-Resource":  in.res,
		}))
	}
	snap := s.Snapshot()
	require.Len(t, snap, 4)
	got := make([]string, len(snap))
	for i, o := range snap {
		got[i] = o.Identity + "/" + o.Resource
	}
	assert.Equal(t, []string{"app:1/core", "app:1/graphql", "user:9/core", "user:9/search"}, got)
}

// TestPrune_PastResetDies: an observation whose reset moment has passed is
// dead — an active identity would have been re-observed with a fresh future
// reset — and Snapshot never returns it; entries with future resets survive.
func TestPrune_PastResetDies(t *testing.T) {
	s := New()
	now := time.Unix(1_800_000_000, 0)
	advance := pinned(s, now)

	s.Observe("user:soon", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
		"X-RateLimit-Reset":     fmt.Sprint(now.Unix() + 60),
	}))
	s.Observe("user:later", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
		"X-RateLimit-Reset":     fmt.Sprint(now.Unix() + 3600),
	}))
	require.Len(t, s.Snapshot(), 2)

	// At the exact reset second the entry still stands (prune is strictly
	// after); one second past, it's gone.
	advance(now.Add(60 * time.Second))
	require.Len(t, s.Snapshot(), 2)
	advance(now.Add(61 * time.Second))
	snap := s.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "user:later", snap[0].Identity)
}

// TestPrune_ZeroResetAgesOutAfterAnHour: a zero Reset (header absent) has no
// window to judge by — the entry is neither immortal nor instantly dead: it
// survives while ObservedAt is within staleTTL and is pruned past it.
func TestPrune_ZeroResetAgesOutAfterAnHour(t *testing.T) {
	s := New()
	now := time.Unix(1_800_000_000, 0)
	advance := pinned(s, now)

	s.Observe("user:noreset", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
	}))
	require.Len(t, s.Snapshot(), 1, "a zero reset must not mean instant death")

	advance(now.Add(59 * time.Minute))
	require.Len(t, s.Snapshot(), 1, "still within the 1h fallback bound")

	advance(now.Add(61 * time.Minute))
	assert.Empty(t, s.Snapshot(), "a zero reset must not mean immortal")
}

// TestPrune_PiggybacksOnObserve: the write side prunes too — a dead entry is
// dropped from the map by an unrelated Observe, without waiting for a read.
func TestPrune_PiggybacksOnObserve(t *testing.T) {
	s := New()
	now := time.Unix(1_800_000_000, 0)
	advance := pinned(s, now)

	s.Observe("user:dead", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
		"X-RateLimit-Reset":     fmt.Sprint(now.Unix() + 10),
	}))

	advance(now.Add(time.Hour))
	s.Observe("user:live", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4999",
		"X-RateLimit-Reset":     fmt.Sprint(now.Add(2 * time.Hour).Unix()),
	}))

	s.mu.Lock()
	_, deadExists := s.obs[key{identity: "user:dead", resource: "core"}]
	size := len(s.obs)
	s.mu.Unlock()
	assert.False(t, deadExists, "Observe must sweep dead entries")
	assert.Equal(t, 1, size)
}

// TestNilStore_Safe: a nil *Store no-ops (the nil-recorder pattern), so
// wiring may pass a nil meter without guards.
func TestNilStore_Safe(t *testing.T) {
	var s *Store
	s.Observe("user:42", respWith(map[string]string{
		"X-RateLimit-Limit":     "10",
		"X-RateLimit-Remaining": "9",
	}))
	assert.Nil(t, s.Snapshot())
}

// TestObserve_Concurrent: parallel observers on overlapping keys must be safe
// (run under -race).
func TestObserve_Concurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Observe(fmt.Sprintf("user:%d", i%10), respWith(map[string]string{
					"X-RateLimit-Limit":     "5000",
					"X-RateLimit-Remaining": fmt.Sprint(5000 - i),
				}))
				_ = s.Snapshot()
			}
		}(g)
	}
	wg.Wait()
	assert.Len(t, s.Snapshot(), 10)
}
