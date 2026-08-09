package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// Notifier tests for the DELIVERY machinery rather than the payload: the
// non-blocking contract, retries and auto-disable, drain, nil safety, and
// the timeline records each attempt leaves.

func TestNotifierNonBlocking(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	start := time.Now()
	n.NotifyIngest(prEvent("my-org", "repo1", 3, "fff"), applied(), time.Now())
	returned := time.Since(start)

	// The delivery is genuinely in flight (the receiver was reached) while
	// NotifyIngest already returned.
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery never reached the receiver")
	}
	assert.Less(t, returned, time.Second, "NotifyIngest must return without waiting on the receiver")

	close(release)
	require.True(t, n.Flush(5*time.Second))
	counters, _ := n.Activity(1)
	assert.Equal(t, int64(1), counters.Delivered, "the released delivery completes")
}

// staticDispatcher answers every webhook dispatch with a fixed result.
type staticDispatcher struct{ result webhook.DispatchResult }

func (d staticDispatcher) Dispatch(context.Context, webhook.Event) webhook.DispatchResult {
	return d.result
}

// TestWebhookHandlerNonBlocking drives the REAL webhook handler with a
// notifier whose receiver blocks: the handler's HTTP response must complete
// while the subscriber endpoint is still hanging.
func TestWebhookHandlerNonBlocking(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var deliveredBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		once.Do(func() {
			deliveredBody = b
			close(arrived)
		})
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	const ghSecret = "gh-webhook-secret"
	handler := webhook.Handler(ghSecret, staticDispatcher{result: applied()}, nil, n)

	body := `{"action":"opened","pull_request":{"number":9,"head":{"sha":"beef"}},"repository":{"name":"repo1","owner":{"login":"my-org"}}}`
	mac := hmac.New(sha256.New, []byte(ghSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "gh-guid")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req) // must return with the receiver still blocked
	assert.Equal(t, http.StatusOK, w.Code, "the webhook response reflects the dispatch, not the subscriber")

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery never reached the receiver")
	}

	close(release)
	require.True(t, n.Flush(5*time.Second))
	counters, _ := n.Activity(1)
	assert.Equal(t, int64(1), counters.Delivered)
	var note Notification
	require.NoError(t, json.Unmarshal(deliveredBody, &note))
	assert.Equal(t, "gh-guid", note.GitHubDelivery, "the mirror's received GUID rides the payload")
	assert.Equal(t, "my-org/repo1", note.RepoFullName)
}

func TestNotifierRetryAndAutoDisable(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, func(c *Config) {
		c.Attempts = 3
		c.Backoff = []time.Duration{time.Millisecond}
		c.DisableAfter = 10
	})

	rec := &capture{respond: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	ctx := context.Background()
	sub, err := st.Create(ctx, "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	// One delivery = 3 attempts = ONE terminal failure.
	n.NotifyIngest(prEvent("my-org", "repo1", 1, "s1"), applied(), time.Now())
	require.True(t, n.Flush(10*time.Second))
	assert.EqualValues(t, 3, rec.requests.Load(), "3 attempts per delivery")
	got, err := st.Get(ctx, "user:1", sub.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.ConsecutiveFailures, "consecutive_failures climbs by 1 per delivery")
	assert.True(t, got.Active)
	assert.Equal(t, "http 500", got.LastError)

	// Nine more terminal failures reach the threshold and auto-disable.
	for i := 0; i < 9; i++ {
		n.NotifyIngest(prEvent("my-org", "repo1", i+2, "s2"), applied(), time.Now())
		require.True(t, n.Flush(10*time.Second))
	}
	got, err = st.Get(ctx, "user:1", sub.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(10), got.ConsecutiveFailures)
	assert.False(t, got.Active, "reaching 10 consecutive failures auto-disables")
	assert.Equal(t, "auto-disabled after 10 consecutive delivery failures", got.DisabledReason)

	counters, _ := n.Activity(0)
	assert.Equal(t, int64(10), counters.Failed)
	assert.Equal(t, int64(1), counters.AutoDisabled)

	// Further deliveries skip the disabled subscription entirely.
	before := rec.requests.Load()
	n.NotifyIngest(prEvent("my-org", "repo1", 99, "s3"), applied(), time.Now())
	require.True(t, n.Flush(10*time.Second))
	assert.Equal(t, before, rec.requests.Load(), "a disabled subscription receives nothing")
}

// TestNotifierDrainCompletesInFlight proves an in-flight delivery finishes —
// and records its outcome — before Drain returns.
func TestNotifierDrainCompletesInFlight(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx := context.Background()
	sub, err := st.Create(ctx, "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(prEvent("my-org", "repo1", 5, "abc"), applied(), time.Now())
	<-arrived

	// Release the receiver shortly AFTER Drain begins waiting.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	require.True(t, n.Drain(5*time.Second), "Drain waits out the in-flight delivery")

	// The delivery completed and its outcome write landed before Drain returned.
	counters, _ := n.Activity(1)
	assert.Equal(t, int64(1), counters.Delivered)
	got, err := st.Get(ctx, "user:1", sub.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, got.LastSuccessAt)

	// After Drain, new notifications are refused outright.
	n.NotifyIngest(prEvent("my-org", "repo1", 6, "def"), applied(), time.Now())
	require.True(t, n.Flush(time.Second))
	counters, _ = n.Activity(1)
	assert.Equal(t, int64(1), counters.Delivered, "a drained notifier accepts no new work")
}

// TestNotifierNilSafe pins the inert-when-nil contract the wiring relies on.
func TestNotifierNilSafe(t *testing.T) {
	var n *Notifier
	n.NotifyIngest(prEvent("o", "r", 1, "s"), applied(), time.Now())
	assert.True(t, n.Drain(time.Millisecond))
	assert.True(t, n.Flush(time.Millisecond))
	assert.Nil(t, n.Store())
	counters, recent := n.Activity(5)
	assert.Zero(t, counters)
	assert.Nil(t, recent)
	subs, err := n.AllSubscriptions(context.Background())
	assert.NoError(t, err)
	assert.Nil(t, subs)
}

// TestNotifierRecordsTimelineAttempts: every outbound delivery attempt lands
// on the timeline ring with its real duration — a failed non-final attempt, a
// failed FINAL attempt, and a delivered one — target host only (never the
// full URL).
func TestNotifierRecordsTimelineAttempts(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	tl := reqtimeline.New()

	// First subscription answers 500 on every attempt (2 attempts, both
	// failed, second final); then a second delivery succeeds first try.
	rec := &capture{respond: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	n, st := newTestNotifier(t, access, func(c *Config) {
		c.Attempts = 2
		c.Backoff = []time.Duration{time.Millisecond}
		c.Timeline = tl
	})
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(prEvent("my-org", "repo1", 1, "s1"), applied(), time.Now())
	require.True(t, n.Flush(10*time.Second))

	snap := tl.Snapshot(0)
	require.Len(t, snap.Events, 2, "one timeline event per real attempt")
	first, second := snap.Events[0], snap.Events[1]
	assert.Equal(t, reqtimeline.KindNotify, first.Kind)
	assert.Equal(t, "⇒ notify", first.Lane)
	assert.Equal(t, "failed", first.Disposition)
	assert.Equal(t, 1, first.Attempt)
	assert.False(t, first.Final, "attempt 1 of 2 is not terminal")
	assert.Equal(t, http.StatusInternalServerError, first.Status)
	assert.Equal(t, "failed", second.Disposition)
	assert.Equal(t, 2, second.Attempt)
	assert.True(t, second.Final, "the last retry is terminal")
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, u.Host, first.Target, "target is the host only, never the full URL")

	// A delivered attempt records final=true, disposition delivered.
	rec.respond = http.StatusOK
	n.NotifyIngest(prEvent("my-org", "repo1", 2, "s2"), applied(), time.Now())
	require.True(t, n.Flush(10*time.Second))
	snap = tl.Snapshot(2)
	require.Len(t, snap.Events, 1)
	assert.Equal(t, "delivered", snap.Events[0].Disposition)
	assert.True(t, snap.Events[0].Final)
	assert.Equal(t, http.StatusOK, snap.Events[0].Status)
	assert.GreaterOrEqual(t, snap.Events[0].DurMs, int64(0))
}
