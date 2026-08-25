package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// replayStore is an in-memory stand-in for the real two-column replay bookkeeping table.
type replayStore struct {
	mu       sync.Mutex
	asked    map[int64]bool
	pruned   []time.Time
	readErr  error
	writeErr error
}

func newReplayStore() *replayStore { return &replayStore{asked: map[int64]bool{}} }

func (s *replayStore) WebhookReplayRequested(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[id], s.readErr
}

func (s *replayStore) RecordWebhookReplay(_ context.Context, id int64, _, _ string, _, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.asked[id] = true
	return nil
}

func (s *replayStore) PruneWebhookReplays(_ context.Context, cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruned = append(s.pruned, cutoff)
	return nil
}

// fakeHooks is GitHub's App-level delivery log plus its redelivery endpoint.
type fakeHooks struct {
	mu         sync.Mutex
	failures   []map[string]any
	redelivers []int64
	listStatus int  // non-zero to fail the list
	attemptErr bool // 500 the redelivery request
	listQuery  string
}

func (f *fakeHooks) server(t *testing.T) *ghclient.AppAuthenticator {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/hook/deliveries", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.listQuery = r.URL.RawQuery
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(f.failures)
	})
	mux.HandleFunc("/app/hook/deliveries/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var id int64
		trimmed := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/hook/deliveries/"), "/attempts")
		_, _ = fmt.Sscanf(trimmed, "%d", &id)
		if f.attemptErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.redelivers = append(f.redelivers, id)
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), ghclient.NewWithBaseURL(srv.URL))
	require.NoError(t, err)
	return app
}

func (f *fakeHooks) requested() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.redelivers...)
}

func failure(id int64, event string, age time.Duration) map[string]any {
	return map[string]any{
		"id": id, "guid": fmt.Sprintf("guid-%d", id),
		"delivered_at": time.Now().Add(-age).UTC().Format(time.RFC3339),
		"status":       "Couldn't connect to server", "status_code": 0,
		"event": event, "action": nil, "redelivery": false,
	}
}

func TestDeliveryReplayer_RequestsUnseenFailures(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{
		failure(1, "push", time.Minute),
		failure(2, "pull_request", 2*time.Minute),
	}}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Minute)

	assert.Equal(t, 2, r.RunOnce(context.Background()))
	assert.ElementsMatch(t, []int64{1, 2}, gh.requested())
	assert.Contains(t, gh.listQuery, "status=failure", "only the failures are ours to replay")
}

// GitHub keeps a failed delivery listed forever, so "already asked" has to be
// remembered -- otherwise every cycle re-requests the same delivery and one
// lost push becomes a permanent replay loop.
func TestDeliveryReplayer_AsksOncePerDelivery(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{failure(7, "push", time.Minute)}}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Minute)
	ctx := context.Background()

	require.Equal(t, 1, r.RunOnce(ctx))
	assert.Equal(t, 0, r.RunOnce(ctx), "a second cycle must not re-request it")
	assert.Equal(t, 0, r.RunOnce(ctx))
	assert.Equal(t, []int64{7}, gh.requested())
}

// Past the lookback the caches a delivery would have moved have expired on
// their own, so replaying it applies state nothing is serving stale.
func TestDeliveryReplayer_SkipsFailuresPastTheLookback(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{
		failure(1, "push", ReplayLookback+time.Hour),
		failure(2, "push", time.Hour),
	}}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Minute)

	require.Equal(t, 1, r.RunOnce(context.Background()))
	assert.Equal(t, []int64{2}, gh.requested())
}

// A burst must not become a flood of replays. What the cap leaves behind is
// taken by the next cycle -- and nothing is dropped silently.
func TestDeliveryReplayer_CapsOneCycleAndResumesOnTheNext(t *testing.T) {
	var failures []map[string]any
	for i := 1; i <= ReplayPerCycle+5; i++ {
		failures = append(failures, failure(int64(i), "push", time.Minute))
	}
	gh := &fakeHooks{failures: failures}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Minute)
	ctx := context.Background()

	assert.Equal(t, ReplayPerCycle, r.RunOnce(ctx))
	assert.Equal(t, 5, r.RunOnce(ctx), "the remainder is taken by the next cycle")
	assert.Len(t, gh.requested(), ReplayPerCycle+5)
	assert.Equal(t, 0, r.RunOnce(ctx))
}

// A redelivery GitHub refuses stays recorded as asked. The request may well
// have been accepted before the error surfaced, and a repeat every cycle is
// the worse failure; the delivery stays in GitHub's own failure log for an
// operator to see, and the error is logged.
func TestDeliveryReplayer_RefusedRequestIsNotRetriedInALoop(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{failure(9, "push", time.Minute)}, attemptErr: true}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Minute)
	ctx := context.Background()

	assert.Equal(t, 0, r.RunOnce(ctx), "a refused request is not a replay")
	assert.Equal(t, 0, r.RunOnce(ctx))
	assert.Empty(t, gh.requested())
}

// The bookkeeping write is what makes "ask once" true, so a failed write must
// stop the request rather than let it go out unrecorded.
func TestDeliveryReplayer_UnrecordableRequestIsNotSent(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{failure(3, "push", time.Minute)}}
	store := newReplayStore()
	store.writeErr = assertAnError
	r := NewDeliveryReplayer(gh.server(t), store, time.Minute)

	assert.Equal(t, 0, r.RunOnce(context.Background()))
	assert.Empty(t, gh.requested())
}

func TestDeliveryReplayer_ListFailureIsNotFatal(t *testing.T) {
	gh := &fakeHooks{listStatus: http.StatusInternalServerError}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Minute)
	assert.Equal(t, 0, r.RunOnce(context.Background()))
}

// Without an App there is no credential for the failure log. Inert, never a
// panic -- the operator warning is emitted at startup instead.
func TestDeliveryReplayer_InertWithoutAnApp(t *testing.T) {
	assert.Equal(t, 0, NewDeliveryReplayer(nil, newReplayStore(), time.Minute).RunOnce(context.Background()))
	assert.Equal(t, 0, (*DeliveryReplayer)(nil).RunOnce(context.Background()))

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewDeliveryReplayer(nil, newReplayStore(), time.Minute).Start(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start must return immediately with no app rather than tick forever")
	}
}

// A zero interval is the operator switching recovery off; it must not run a
// startup cycle either.
func TestDeliveryReplayer_ZeroIntervalDisables(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{failure(1, "push", time.Minute)}}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), 0)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start must return immediately when disabled")
	}
	assert.Empty(t, gh.requested())
}

// The startup cycle is the point: a restart is itself a window in which
// deliveries fail, so a fresh process asks what it missed straight away
// rather than an interval later.
func TestDeliveryReplayer_StartRunsACycleImmediately(t *testing.T) {
	gh := &fakeHooks{failures: []map[string]any{failure(11, "push", time.Minute)}}
	r := NewDeliveryReplayer(gh.server(t), newReplayStore(), time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx) }()

	require.Eventually(t, func() bool { return len(gh.requested()) == 1 }, 3*time.Second, 10*time.Millisecond)
	cancel()
	<-done
}

var assertAnError = fmt.Errorf("bookkeeping is unavailable")
