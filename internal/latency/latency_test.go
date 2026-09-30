package latency

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_PercentilesAndBuckets(t *testing.T) {
	r := New()
	for range 90 {
		r.Observe("db.exec", 800*time.Microsecond) // bucket <= 1ms
	}
	for range 9 {
		r.Observe("db.exec", 40*time.Millisecond) // bucket <= 50ms
	}
	r.Observe("db.exec", 45*time.Second) // overflow bucket

	s := r.Snapshot()
	require.Len(t, s.Kinds, 1)
	k := s.Kinds[0]
	assert.Equal(t, "db.exec", k.Kind)
	assert.Equal(t, int64(100), k.Count)
	assert.Equal(t, 1.0, k.P50Ms)
	assert.Equal(t, 50.0, k.P95Ms)
	assert.Equal(t, 50.0, k.P99Ms)
	assert.Equal(t, 45000.0, k.MaxMs)
	require.Len(t, k.Buckets, len(Bounds)+1)
	assert.Equal(t, int64(1), k.Buckets[len(Bounds)], "an observation past the last edge lands in the overflow bucket")
	var total int64
	for _, c := range k.Buckets {
		total += c
	}
	assert.Equal(t, k.Count, total, "no observation is dropped")
}

func TestRecorder_PercentileCappedAtMax(t *testing.T) {
	r := New()
	r.Observe("x", 3*time.Millisecond)
	assert.Equal(t, 3.0, r.Snapshot().Kinds[0].P99Ms, "the bucket edge (5ms) is above anything observed")
}

func TestRecorder_NilIsSafe(t *testing.T) {
	var r *Recorder
	r.Observe("x", time.Second)
	assert.Empty(t, r.Snapshot().Kinds)
}
