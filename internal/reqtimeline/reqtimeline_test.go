package reqtimeline

import (
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

// newTestRecorder returns a Recorder with an injectable clock.
func newTestRecorder(retention time.Duration, maxEvents int, now *time.Time) *Recorder {
	return &Recorder{retention: retention, maxEvents: maxEvents, now: func() time.Time { return *now }}
}

func TestRecordAndSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 100, &now)

	r.RecordWebhook(now.Add(-time.Second), 40*time.Millisecond, "push", "", "d-1", "o/r", "applied")
	r.RecordRequest(now.Add(-500*time.Millisecond), 320*time.Millisecond, "GET", "/repos/{owner}/{repo}/pulls", 200, "passthrough", "user:1", "octocat")

	snap := r.Snapshot(0)
	require.Equal(t, 2, len(snap.Events))

	require.Equal(t, uint64(2), snap.MaxID)

	wh, rq := snap.Events[0], snap.Events[1]
	require.False(t, wh.Kind != KindWebhook || wh.Lane != "⇐ push" || wh.DurMs != 40 || wh.EventType != "push" || wh.Repo != "o/r" || wh.DeliveryID != "d-1" || wh.Disposition != "applied")

	require.False(t, rq.Kind != KindRequest || rq.Lane != "GET /repos/{owner}/{repo}/pulls" || rq.DurMs != 320 || rq.Status != 200 || rq.Actor != "user:1" || rq.ActorName != "octocat" || rq.Disposition != "passthrough")

	require.False(t, wh.ID != 1 || rq.ID != 2)

	got, want := snap.RetentionStart, now.Add(-24*time.Hour)
	require.True(t, got.Equal(want))

}

func TestSinceCursor(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 100, &now)
	for i := 0; i < 5; i++ {
		r.RecordWebhook(now, time.Millisecond, "push", "", "", "o/r", "applied")
	}

	snap := r.Snapshot(3)
	require.Equal(t, 2, len(snap.Events))

	require.False(t, snap.Events[0].ID != 4 || snap.Events[1].ID != 5)

	// A cursor at (or past) the newest ID yields an empty page but MaxID
	// still reports the frontier.
	snap = r.Snapshot(5)
	require.False(t, len(snap.Events) != 0 || snap.MaxID != 5)

	snap = r.Snapshot(99)
	require.Equal(t, 0, len(snap.Events))

}

func TestRetentionEviction(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 100, &now)

	r.RecordWebhook(now.Add(-30*time.Hour), 10*time.Millisecond, "push", "", "", "o/r", "applied")
	r.RecordWebhook(now.Add(-time.Hour), 10*time.Millisecond, "push", "", "", "o/r", "applied")

	// Eviction is lazy on read: the 30h-old event is gone, the 1h-old stays.
	snap := r.Snapshot(0)
	require.False(t, len(snap.Events) != 1 || snap.Events[0].ID != 2)

	// Advance the clock past the survivor's window: read-side eviction drops
	// it too, with no write in between (lazy on read, not only on write).
	now = now.Add(25 * time.Hour)
	snap = r.Snapshot(0)
	require.Equal(t, 0, len(snap.Events))

	require.Equal(t, uint64(2), snap.MaxID)

}

// The retention clock runs on END time: a long-running event whose START
// predates the window survives while its end is inside it.
func TestRetentionUsesEndTime(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(time.Hour, 100, &now)
	// Started 90m ago, ran 45m: ended 45m ago — inside the 1h window.
	r.RecordRequest(now.Add(-90*time.Minute), 45*time.Minute, "GET", "/x", 200, "passthrough", "a", "")
	snap := r.Snapshot(0)
	require.Equal(t, 1, len(snap.Events))

}

func TestCountCap(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 10, &now)
	for i := 0; i < 25; i++ {
		r.RecordWebhook(now, time.Millisecond, "push", "", "", "o/r", "applied")
	}
	snap := r.Snapshot(0)
	require.Equal(t, 10, len(snap.Events))

	require.False(t, snap.Events[0].ID != 16 || snap.Events[9].ID != 25)

}

// Compaction (the amortized head-reset) must not corrupt the live window.
func TestCompactionKeepsLiveEvents(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 2000, &now)
	// Fill beyond the compaction threshold (head > 1024) with events already
	// outside retention, so every write advances head and compaction fires.
	for i := 0; i < 3000; i++ {
		r.RecordWebhook(now.Add(-25*time.Hour), time.Millisecond, "old", "", "", "o/r", "applied")
	}
	// Everything so far is instantly outside retention; head grows and
	// compaction has triggered at least once. Now record live events.
	for i := 0; i < 5; i++ {
		r.RecordWebhook(now, time.Millisecond, "push", "", "", "o/r", "applied")
	}
	snap := r.Snapshot(0)
	require.Equal(t, 5, len(snap.Events))

	for i, e := range snap.Events {
		want := uint64(3001 + i)
		require.Equal(t, want, e.ID)

		require.Equal(t, "push", e.EventType)

	}
}

func TestNilRecorderSafe(t *testing.T) {
	var r *Recorder
	// Must not panic.
	r.RecordWebhook(time.Now(), time.Millisecond, "push", "", "", "", "applied")
	r.RecordRequest(time.Now(), time.Millisecond, "GET", "/x", 200, "passthrough", "a", "")
	snap := r.Snapshot(0)
	require.False(t, snap.Events == nil || len(snap.Events) != 0)

}

func TestConcurrentAccess(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.RecordWebhook(start, time.Millisecond, "push", "opened", "d", "o/r", "applied")
				r.RecordRequest(start, time.Millisecond, "GET", "/x", 200, "hit", "a", "")
				_ = r.Snapshot(0)
			}
		}()
	}
	wg.Wait()
	snap := r.Snapshot(0)
	require.Equal(t, 8000, len(snap.Events))

	require.Equal(t, uint64(8000), snap.MaxID)

	// IDs must be strictly increasing in the ring.
	for i := 1; i < len(snap.Events); i++ {
		require.Greater(t, snap.Events[i].ID, snap.Events[i-1].ID)

	}
}
