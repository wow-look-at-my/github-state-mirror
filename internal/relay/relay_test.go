package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

const testSecret = "relay-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type staticDispatcher struct{}

func (staticDispatcher) Dispatch(context.Context, webhook.Event) webhook.DispatchResult {
	return webhook.DispatchResult{Disposition: webhook.DispApplied}
}

// fakeTarget records each request it receives and answers with statuses in turn; the last repeats.
type fakeTarget struct {
	srv      *httptest.Server
	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	statuses []int
	hits     atomic.Int32
}

func newFakeTarget(t *testing.T, statuses ...int) *fakeTarget {
	f := &fakeTarget{statuses: statuses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := int(f.hits.Add(1))
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		status := f.statuses[min(n, len(f.statuses))-1]
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestRelay(t *testing.T, cfg Config) *Relay {
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = 10 * time.Millisecond
	}
	r := New(cfg)
	require.NotNil(t, r)
	t.Cleanup(func() { r.Drain(time.Second) })
	return r
}

// captureLogs routes slog to a buffer for the test.
func captureLogs(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func githubRequest(body []byte, deliveryID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/abc123")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-GitHub-Hook-ID", "4242")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "99")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-Hub-Signature", "sha1=0000")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	req.Header.Set("Authorization", "Bearer must-not-travel")
	return req
}

func TestRelay_ForwardsBodyAndGitHubHeadersExactly(t *testing.T) {
	target := newFakeTarget(t, http.StatusOK)
	tl := reqtimeline.New()
	r := newTestRelay(t, Config{Targets: []string{target.srv.URL + "/webhook"}, Timeline: tl})
	handler := webhook.HandlerWithRelay(testSecret, staticDispatcher{}, nil, r)

	// Whitespace and key order are part of the signed bytes, so they must survive.
	body := []byte("{\"ref\": \"refs/heads/main\",\n  \"after\":\"abc\"}\n")
	req := githubRequest(body, "d-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	require.Eventually(t, func() bool { return r.Stats()[0].Delivered == 1 }, 5*time.Second, 5*time.Millisecond)
	target.mu.Lock()
	defer target.mu.Unlock()
	require.Len(t, target.bodies, 1)
	assert.Equal(t, body, target.bodies[0], "the body must reach the target byte-identical")
	got := target.headers[0]
	for _, name := range ForwardedHeaders {
		assert.Equal(t, req.Header.Get(name), got.Get(name), "header %s", name)
	}
	assert.Empty(t, got.Get("Authorization"), "only GitHub's own headers travel")

	events := tl.Snapshot(0).Events
	require.Len(t, events, 1)
	assert.Equal(t, "⇒ relay", events[0].Lane)
	assert.Equal(t, "d-1", events[0].DeliveryID)
	assert.Equal(t, http.StatusOK, events[0].Status)
	assert.True(t, events[0].Final)
}

func TestRelay_UnverifiedDeliveryIsNotRelayed(t *testing.T) {
	target := newFakeTarget(t, http.StatusOK)
	r := newTestRelay(t, Config{Targets: []string{target.srv.URL}})
	handler := webhook.HandlerWithRelay(testSecret, staticDispatcher{}, nil, r)

	req := githubRequest([]byte(`{"ref":"x"}`), "d-forged")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)

	require.True(t, r.Drain(time.Second))
	assert.Zero(t, target.hits.Load())
	assert.Zero(t, r.Stats()[0].Delivered)
}

func TestRelay_RetriesServerErrorUntil2xx(t *testing.T) {
	target := newFakeTarget(t, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusOK)
	r := newTestRelay(t, Config{Targets: []string{target.srv.URL}})

	r.RelayDelivery(githubRequest([]byte(`{}`), "d-retry").Header, []byte(`{}`))

	require.Eventually(t, func() bool { return r.Stats()[0].Delivered == 1 }, 5*time.Second, 5*time.Millisecond)
	s := r.Stats()[0]
	assert.Equal(t, uint64(3), s.Retried)
	assert.Zero(t, s.PermanentlyFailed)
	assert.Equal(t, int32(4), target.hits.Load())
	assert.Contains(t, s.LastError, "d-retry")
}

func TestRelay_Permanent4xxStopsRetriesAndIsCounted(t *testing.T) {
	logs := captureLogs(t)
	target := newFakeTarget(t, http.StatusUnprocessableEntity)
	r := newTestRelay(t, Config{Targets: []string{target.srv.URL}})

	r.RelayDelivery(githubRequest([]byte(`{}`), "d-perm").Header, []byte(`{}`))

	require.Eventually(t, func() bool { return r.Stats()[0].PermanentlyFailed == 1 }, 5*time.Second, 5*time.Millisecond)
	// Retry intervals pass with no further attempt.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), target.hits.Load())
	s := r.Stats()[0]
	assert.Zero(t, s.Retried)
	assert.Zero(t, s.Delivered)
	assert.Contains(t, s.LastError, "422")
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), "delivery=d-perm")
}

// blockingTarget holds every request until release runs.
func blockingTarget(t *testing.T) (*httptest.Server, func(), *atomic.Int32) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		release()
		srv.Close()
	})
	return srv, release, &hits
}

func TestRelay_QueueOverflowIsCountedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	srv, release, hits := blockingTarget(t)
	r := newTestRelay(t, Config{Targets: []string{srv.URL}, QueueSize: 1})

	send := func(id string) { r.RelayDelivery(githubRequest([]byte(`{}`), id).Header, []byte(`{}`)) }
	send("d-1")
	require.Eventually(t, func() bool { return hits.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	send("d-2") // fills the queue behind the in-flight d-1
	send("d-3") // no room

	s := r.Stats()[0]
	assert.Equal(t, 1, s.InFlight)
	assert.Equal(t, 1, s.Queued)
	assert.Equal(t, uint64(1), s.Dropped)
	assert.Contains(t, s.LastError, "d-3")
	out := logs.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, "delivery=d-3")
	assert.Contains(t, out, "target="+Display(srv.URL))

	release()
	require.Eventually(t, func() bool { return r.Stats()[0].Delivered == 2 }, 5*time.Second, 5*time.Millisecond)
}

func TestRelay_ResponseDoesNotWaitOnSlowTarget(t *testing.T) {
	srv, _, hits := blockingTarget(t)
	r := newTestRelay(t, Config{Targets: []string{srv.URL}, AttemptTimeout: time.Minute})
	handler := webhook.HandlerWithRelay(testSecret, staticDispatcher{}, nil, r)

	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, githubRequest([]byte(`{}`), "d-slow"))
		done <- w.Code
	}()
	select {
	case code := <-done:
		assert.Equal(t, http.StatusOK, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook response waited on the relay target")
	}
	require.Eventually(t, func() bool { return hits.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, r.Stats()[0].InFlight)
}

func TestRelay_DrainReportsUndelivered(t *testing.T) {
	logs := captureLogs(t)
	target := newFakeTarget(t, http.StatusServiceUnavailable)
	r := New(Config{Targets: []string{target.srv.URL}, RetryInterval: 10 * time.Millisecond})

	r.RelayDelivery(githubRequest([]byte(`{}`), "d-a").Header, []byte(`{}`))
	r.RelayDelivery(githubRequest([]byte(`{}`), "d-b").Header, []byte(`{}`))

	assert.False(t, r.Drain(100*time.Millisecond))
	assert.Contains(t, logs.String(), "undelivered=2")

	// After shutdown a delivery is dropped out loud, never queued for nobody.
	r.RelayDelivery(githubRequest([]byte(`{}`), "d-late").Header, []byte(`{}`))
	assert.Equal(t, uint64(1), r.Stats()[0].Dropped)
	assert.Contains(t, logs.String(), "delivery=d-late")
}

func TestRelay_NilIsANoOp(t *testing.T) {
	var r *Relay
	assert.Nil(t, New(Config{}))
	r.RelayDelivery(http.Header{}, nil)
	assert.Nil(t, r.Stats())
	assert.True(t, r.Drain(time.Millisecond))
}

func TestDisplayHidesCredentials(t *testing.T) {
	assert.Equal(t, "https://host.example/hook", Display("https://user:pw@host.example/hook?token=s3cret"))
	assert.False(t, strings.Contains(Display("https://host.example/x?token=s3cret"), "s3cret"))
}
