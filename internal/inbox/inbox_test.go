package inbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

const secret = "test-secret"

type recordingDispatcher struct {
	mu      sync.Mutex
	ids     []string
	release chan struct{}
}

func (d *recordingDispatcher) Dispatch(_ context.Context, event webhook.Event) webhook.DispatchResult {
	if d.release != nil {
		<-d.release
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, event.DeliveryID)
	return webhook.DispatchResult{Event: event.Type, Disposition: webhook.DispApplied}
}

func (d *recordingDispatcher) applied() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func runQueue(t *testing.T, q *webhook.Queue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	t.Cleanup(func() {
		cancel()
		q.Stopped(5 * time.Second)
	})
}

func post(t *testing.T, h http.Handler, deliveryID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"action":"completed","repository":{"name":"repo","owner":{"login":"org"}}}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "check_run")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// GitHub is answered as soon as the delivery is recorded, while its apply is still waiting.
func TestHandler_AnswersOnceRecordedNotOnceApplied(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "inbox.db"))
	dispatcher := &recordingDispatcher{release: make(chan struct{})}
	q := webhook.NewQueue(store, dispatcher, nil)
	runQueue(t, q)
	h := webhook.Handler(secret, q)

	w := post(t, h, "d-1")
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, webhook.DispQueued, w.Header().Get("X-GSM-Disposition"))
	assert.Empty(t, dispatcher.applied(), "answered before the apply ran")

	pending, err := store.Pending(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the answered delivery is in the journal")
	assert.Equal(t, "d-1", pending[0].DeliveryID)

	close(dispatcher.release)
	require.True(t, q.Idle(5*time.Second))
	assert.Equal(t, []string{"d-1"}, dispatcher.applied())
}

// A delivery id seen before is answered as a duplicate and applied once,
// both while the first copy is queued and after it was applied.
func TestHandler_RedeliveryIsAppliedOnce(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "inbox.db"))
	dispatcher := &recordingDispatcher{release: make(chan struct{})}
	q := webhook.NewQueue(store, dispatcher, nil)
	runQueue(t, q)
	h := webhook.Handler(secret, q)

	require.Equal(t, webhook.DispQueued, post(t, h, "d-dup").Header().Get("X-GSM-Disposition"))
	w := post(t, h, "d-dup")
	assert.Less(t, w.Code, 300, "a repeat is still a success to GitHub")
	assert.Equal(t, webhook.DispDuplicate, w.Header().Get("X-GSM-Disposition"))

	close(dispatcher.release)
	require.True(t, q.Idle(5*time.Second))
	assert.Equal(t, webhook.DispDuplicate, post(t, h, "d-dup").Header().Get("X-GSM-Disposition"))
	require.True(t, q.Idle(5*time.Second))
	assert.Equal(t, []string{"d-dup"}, dispatcher.applied())
}

// A delivery recorded and answered, then lost to a crash before its apply, is
// applied by the next process's worker.
func TestQueue_AppliesWhatACrashLeftRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	first, err := Open(path)
	require.NoError(t, err)
	stuck := &recordingDispatcher{release: make(chan struct{})}
	h := webhook.Handler(secret, webhook.NewQueue(first, stuck, nil))
	require.Equal(t, webhook.DispQueued, post(t, h, "d-crash").Header().Get("X-GSM-Disposition"))
	require.NoError(t, first.Close(), "the process dies with no worker having applied it")
	assert.Empty(t, stuck.applied())

	store := openStore(t, path)
	dispatcher := &recordingDispatcher{}
	q := webhook.NewQueue(store, dispatcher, nil)
	runQueue(t, q)
	require.True(t, q.Idle(5*time.Second))
	assert.Equal(t, []string{"d-crash"}, dispatcher.applied())

	pending, err := store.Pending(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// Deliveries are applied in the order they were recorded.
func TestQueue_AppliesInArrivalOrder(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "inbox.db"))
	dispatcher := &recordingDispatcher{}
	q := webhook.NewQueue(store, dispatcher, nil)
	h := webhook.Handler(secret, q)
	want := []string{"d-a", "d-b", "d-c", "d-d"}
	for _, id := range want {
		require.Equal(t, webhook.DispQueued, post(t, h, id).Header().Get("X-GSM-Disposition"))
	}
	runQueue(t, q)
	require.True(t, q.Idle(5*time.Second))
	assert.Equal(t, want, dispatcher.applied())
}

// Applied ids past Retention are dropped; younger ones still deduplicate.
func TestStore_PrunesAppliedPastRetention(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "inbox.db"))
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"old", "young"} {
		fresh, err := store.Put(ctx, webhook.Stored{DeliveryID: id, EventType: "push", Body: []byte(`{}`), ReceivedAt: now})
		require.NoError(t, err)
		require.True(t, fresh)
	}
	require.NoError(t, store.MarkApplied(ctx, "old", now.Add(-Retention-time.Hour)))
	store.lastPrune.Store(0)
	require.NoError(t, store.MarkApplied(ctx, "young", now))

	fresh, err := store.Put(ctx, webhook.Stored{DeliveryID: "old", EventType: "push", Body: []byte(`{}`), ReceivedAt: now})
	require.NoError(t, err)
	assert.True(t, fresh, "an id past retention is forgotten")
	fresh, err = store.Put(ctx, webhook.Stored{DeliveryID: "young", EventType: "push", Body: []byte(`{}`), ReceivedAt: now})
	require.NoError(t, err)
	assert.False(t, fresh, "a recent id is still a duplicate")
}
