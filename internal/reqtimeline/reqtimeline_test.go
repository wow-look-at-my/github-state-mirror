package reqtimeline

import (
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
	if len(snap.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(snap.Events))
	}
	if snap.MaxID != 2 {
		t.Fatalf("want MaxID 2, got %d", snap.MaxID)
	}
	wh, rq := snap.Events[0], snap.Events[1]
	if wh.Kind != KindWebhook || wh.Lane != "⇐ push" || wh.DurMs != 40 || wh.EventType != "push" ||
		wh.Repo != "o/r" || wh.DeliveryID != "d-1" || wh.Disposition != "applied" {
		t.Fatalf("webhook event mismatch: %+v", wh)
	}
	if rq.Kind != KindRequest || rq.Lane != "GET /repos/{owner}/{repo}/pulls" || rq.DurMs != 320 ||
		rq.Status != 200 || rq.Actor != "user:1" || rq.ActorName != "octocat" || rq.Disposition != "passthrough" {
		t.Fatalf("request event mismatch: %+v", rq)
	}
	if wh.ID != 1 || rq.ID != 2 {
		t.Fatalf("IDs must be monotonic from 1: %d, %d", wh.ID, rq.ID)
	}
	if got, want := snap.RetentionStart, now.Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("retention start: got %v want %v", got, want)
	}
}

func TestSinceCursor(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 100, &now)
	for i := 0; i < 5; i++ {
		r.RecordWebhook(now, time.Millisecond, "push", "", "", "o/r", "applied")
	}

	snap := r.Snapshot(3)
	if len(snap.Events) != 2 {
		t.Fatalf("since=3: want 2 events, got %d", len(snap.Events))
	}
	if snap.Events[0].ID != 4 || snap.Events[1].ID != 5 {
		t.Fatalf("since=3: want IDs 4,5 got %d,%d", snap.Events[0].ID, snap.Events[1].ID)
	}
	// A cursor at (or past) the newest ID yields an empty page but MaxID
	// still reports the frontier.
	snap = r.Snapshot(5)
	if len(snap.Events) != 0 || snap.MaxID != 5 {
		t.Fatalf("since=5: want 0 events MaxID 5, got %d events MaxID %d", len(snap.Events), snap.MaxID)
	}
	snap = r.Snapshot(99)
	if len(snap.Events) != 0 {
		t.Fatalf("since past the frontier must be empty, got %d", len(snap.Events))
	}
}

func TestRetentionEviction(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 100, &now)

	r.RecordWebhook(now.Add(-30*time.Hour), 10*time.Millisecond, "push", "", "", "o/r", "applied")
	r.RecordWebhook(now.Add(-time.Hour), 10*time.Millisecond, "push", "", "", "o/r", "applied")

	// Eviction is lazy on read: the 30h-old event is gone, the 1h-old stays.
	snap := r.Snapshot(0)
	if len(snap.Events) != 1 || snap.Events[0].ID != 2 {
		t.Fatalf("want only event 2 after retention eviction, got %+v", snap.Events)
	}

	// Advance the clock past the survivor's window: read-side eviction drops
	// it too, with no write in between (lazy on read, not only on write).
	now = now.Add(25 * time.Hour)
	snap = r.Snapshot(0)
	if len(snap.Events) != 0 {
		t.Fatalf("want empty after the window passed, got %+v", snap.Events)
	}
	if snap.MaxID != 2 {
		t.Fatalf("MaxID must survive eviction, got %d", snap.MaxID)
	}
}

// The retention clock runs on END time: a long-running event whose START
// predates the window survives while its end is inside it.
func TestRetentionUsesEndTime(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(time.Hour, 100, &now)
	// Started 90m ago, ran 45m: ended 45m ago — inside the 1h window.
	r.RecordRequest(now.Add(-90*time.Minute), 45*time.Minute, "GET", "/x", 200, "passthrough", "a", "")
	if snap := r.Snapshot(0); len(snap.Events) != 1 {
		t.Fatalf("event ending inside the window must survive, got %+v", snap.Events)
	}
}

func TestCountCap(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	r := newTestRecorder(24*time.Hour, 10, &now)
	for i := 0; i < 25; i++ {
		r.RecordWebhook(now, time.Millisecond, "push", "", "", "o/r", "applied")
	}
	snap := r.Snapshot(0)
	if len(snap.Events) != 10 {
		t.Fatalf("count cap: want 10 events, got %d", len(snap.Events))
	}
	if snap.Events[0].ID != 16 || snap.Events[9].ID != 25 {
		t.Fatalf("count cap must drop the OLDEST: got IDs %d..%d", snap.Events[0].ID, snap.Events[9].ID)
	}
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
	if len(snap.Events) != 5 {
		t.Fatalf("want the 5 live events after compaction, got %d", len(snap.Events))
	}
	for i, e := range snap.Events {
		if want := uint64(3001 + i); e.ID != want {
			t.Fatalf("event %d: want ID %d got %d", i, want, e.ID)
		}
		if e.EventType != "push" {
			t.Fatalf("event %d: stale entry leaked through compaction: %+v", i, e)
		}
	}
}

func TestNilRecorderSafe(t *testing.T) {
	var r *Recorder
	// Must not panic.
	r.RecordWebhook(time.Now(), time.Millisecond, "push", "", "", "", "applied")
	r.RecordRequest(time.Now(), time.Millisecond, "GET", "/x", 200, "passthrough", "a", "")
	snap := r.Snapshot(0)
	if snap.Events == nil || len(snap.Events) != 0 {
		t.Fatalf("nil recorder snapshot must be empty-but-non-nil, got %+v", snap.Events)
	}
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
	if len(snap.Events) != 8000 {
		t.Fatalf("want 8000 events after concurrent writes, got %d", len(snap.Events))
	}
	if snap.MaxID != 8000 {
		t.Fatalf("want MaxID 8000, got %d", snap.MaxID)
	}
	// IDs must be strictly increasing in the ring.
	for i := 1; i < len(snap.Events); i++ {
		if snap.Events[i].ID <= snap.Events[i-1].ID {
			t.Fatalf("IDs out of order at %d: %d then %d", i, snap.Events[i-1].ID, snap.Events[i].ID)
		}
	}
}
