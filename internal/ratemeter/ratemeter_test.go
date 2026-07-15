package ratemeter

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

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

func TestObserve_ParsesHeaders(t *testing.T) {
	s := New()
	s.Observe("user:42", "", respWith(map[string]string{
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
	s.Observe("user:42", "", respWith(map[string]string{
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
	s.Observe("user:42", "", respWith(map[string]string{
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
	s.Observe("user:42", "", respWith(nil))
	s.Observe("user:42", "", respWith(map[string]string{"X-RateLimit-Limit": "5000"}))
	s.Observe("user:42", "", respWith(map[string]string{"X-RateLimit-Remaining": "10"}))
	s.Observe("user:42", "", respWith(map[string]string{
		"X-RateLimit-Limit":     "junk",
		"X-RateLimit-Remaining": "10",
	}))
	assert.Empty(t, s.Snapshot())
}

// TestObserve_LastWriteWins: a later reading for the same (identity, resource)
// replaces the earlier one.
func TestObserve_LastWriteWins(t *testing.T) {
	s := New()
	s.Observe("user:42", "", respWith(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
	}))
	s.Observe("user:42", "", respWith(map[string]string{
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
	s.Observe("", "", respWith(map[string]string{
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
		s.Observe(fmt.Sprintf("token:%012d", i), "", respWith(map[string]string{
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
		s.Observe(in.id, "", respWith(map[string]string{
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

// TestNilStore_Safe: a nil *Store no-ops (the nil-recorder pattern), so
// wiring may pass a nil meter without guards.
func TestNilStore_Safe(t *testing.T) {
	var s *Store
	s.Observe("user:42", "", respWith(map[string]string{
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
				s.Observe(fmt.Sprintf("user:%d", i%10), "", respWith(map[string]string{
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

// TestObserve_Name: the verified display name is recorded alongside the
// identity, survives nameless re-observations of the same identity (the key
// pins the principal), and a fresh name overwrites.
func TestObserve_Name(t *testing.T) {
	s := New()
	headers := map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4000",
	}

	s.Observe("app:99", "pr-minder", respWith(headers))
	require.Len(t, s.Snapshot(), 1)
	assert.Equal(t, "pr-minder", s.Snapshot()[0].Name)

	// A nameless observation of the same identity keeps the known name.
	s.Observe("app:99", "", respWith(headers))
	require.Len(t, s.Snapshot(), 1)
	assert.Equal(t, "pr-minder", s.Snapshot()[0].Name, "a nameless reading must not erase the known name")

	// A new verified name overwrites (e.g. an app was renamed).
	s.Observe("app:99", "pr-minder-2", respWith(headers))
	assert.Equal(t, "pr-minder-2", s.Snapshot()[0].Name)

	// An identity never observed with a name has none.
	s.Observe("token:abc", "", respWith(headers))
	for _, o := range s.Snapshot() {
		if o.Identity == "token:abc" {
			assert.Equal(t, "", o.Name)
		}
	}
}
