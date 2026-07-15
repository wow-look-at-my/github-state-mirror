package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingDispatcher struct {
	mu     sync.Mutex
	events []Event
	result DispatchResult
}

func (d *recordingDispatcher) Dispatch(_ context.Context, event Event) DispatchResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
	return d.result
}

func (d *recordingDispatcher) getEvents() []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Event{}, d.events...)
}

func signPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandler_ValidWebhook(t *testing.T) {
	secret := "test-secret"
	dispatcher := &recordingDispatcher{result: DispatchResult{Disposition: DispApplied}}
	handler := Handler(secret, dispatcher, nil)

	body := `{"action":"opened","pull_request":{"number":42},"repository":{"name":"repo","owner":{"login":"org"}}}`
	sig := signPayload(secret, []byte(body))

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "pull_request")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHandler_NoSecretRejected(t *testing.T) {
	dispatcher := &recordingDispatcher{}
	handler := Handler("", dispatcher, nil)

	body := `{"action":"opened"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Without a configured secret the endpoint must fail closed.
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, dispatcher.getEvents())
}

func TestHandler_InvalidSignature(t *testing.T) {
	secret := "test-secret"
	dispatcher := &recordingDispatcher{}
	handler := Handler(secret, dispatcher, nil)

	body := `{"action":"opened"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Event", "push")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	handler := Handler("", &recordingDispatcher{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandler_MissingEventType(t *testing.T) {
	secret := "test-secret"
	handler := Handler(secret, &recordingDispatcher{}, nil)

	body := `{"action":"opened"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(body)))
	// No X-GitHub-Event header.

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandler_MissingSignature(t *testing.T) {
	secret := "test-secret"
	handler := Handler(secret, &recordingDispatcher{}, nil)

	body := `{"action":"opened"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	// No signature header.

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandler_DispatchSynchronous(t *testing.T) {
	secret := "test-secret"
	dispatcher := &recordingDispatcher{result: DispatchResult{Disposition: DispApplied}}
	handler := Handler(secret, dispatcher, nil)

	body := `{"action":"opened","repository":{"name":"repo","owner":{"login":"org"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-id")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(body)))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Dispatch is synchronous: the event is recorded by the time ServeHTTP
	// returns, and the X-GitHub-Delivery header is threaded through.
	events := dispatcher.getEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "push", events[0].Type)
	assert.Equal(t, "test-delivery-id", events[0].DeliveryID)
}

// recordingDeliveryRecorder captures RecordDelivery calls for assertions.
type recordingDeliveryRecorder struct {
	mu    sync.Mutex
	calls []struct {
		event    Event
		result   DispatchResult
		received bool
		durValid bool
	}
}

func (r *recordingDeliveryRecorder) RecordDelivery(event Event, result DispatchResult, receivedAt time.Time, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		event    Event
		result   DispatchResult
		received bool
		durValid bool
	}{event, result, !receivedAt.IsZero(), duration >= 0})
}

// TestHandler_RecordsDelivery verifies the recorder observes a verified
// delivery with its real fields and a measured (non-negative, non-faked)
// duration — and that the response is unchanged by the recording.
func TestHandler_RecordsDelivery(t *testing.T) {
	secret := "test-secret"
	dispatcher := &recordingDispatcher{result: DispatchResult{Event: "pull_request", Disposition: DispApplied}}
	rec := &recordingDeliveryRecorder{}
	handler := Handler(secret, dispatcher, rec)

	body := `{"action":"opened","pull_request":{"number":42},"repository":{"name":"repo","owner":{"login":"org"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-123")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(body)))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, DispApplied, w.Header().Get("X-GSM-Disposition"))

	require.Len(t, rec.calls, 1)
	call := rec.calls[0]
	assert.Equal(t, "pull_request", call.event.Type)
	assert.Equal(t, "opened", call.event.Action)
	assert.Equal(t, "delivery-123", call.event.DeliveryID)
	assert.Equal(t, "org/repo", call.event.RepoFullName())
	assert.Equal(t, DispApplied, call.result.Disposition)
	assert.True(t, call.received, "receivedAt must be stamped")
	assert.True(t, call.durValid, "duration must be a real non-negative measurement")
}

// TestHandler_RecorderSkipsUnverified: a delivery that fails signature
// verification never reaches the recorder (nothing unverified may leave a
// timeline trace).
func TestHandler_RecorderSkipsUnverified(t *testing.T) {
	rec := &recordingDeliveryRecorder{}
	handler := Handler("test-secret", &recordingDispatcher{}, rec)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, rec.calls)
}

func TestHandler_WritesOutcome(t *testing.T) {
	secret := "test-secret"
	dispatcher := &recordingDispatcher{result: DispatchResult{
		Event:       "pull_request",
		Disposition: DispIgnored,
		Detail:      "action edited not tracked",
	}}
	handler := Handler(secret, dispatcher, nil)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(body)))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// A no-op delivery is a 202 (received, nothing applied), distinct from the
	// 200 of an applied delivery — visible in GitHub's deliveries list.
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, DispIgnored, w.Header().Get("X-GSM-Disposition"))

	var got DispatchResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, DispIgnored, got.Disposition)
	assert.Equal(t, "action edited not tracked", got.Detail)
}
