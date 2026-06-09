package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingDispatcher struct {
	mu	sync.Mutex
	events	[]Event
}

func (d *recordingDispatcher) Dispatch(_ context.Context, event Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
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
	dispatcher := &recordingDispatcher{}
	handler := Handler(secret, dispatcher)

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
	handler := Handler("", dispatcher)

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
	handler := Handler(secret, dispatcher)

	body := `{"action":"opened"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Event", "push")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	handler := Handler("", &recordingDispatcher{})

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandler_MissingEventType(t *testing.T) {
	secret := "test-secret"
	handler := Handler(secret, &recordingDispatcher{})

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
	handler := Handler(secret, &recordingDispatcher{})

	body := `{"action":"opened"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	// No signature header.

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandler_DispatchCalledAsync(t *testing.T) {
	secret := "test-secret"
	dispatcher := &recordingDispatcher{}
	handler := Handler(secret, dispatcher)

	body := `{"action":"opened","repository":{"name":"repo","owner":{"login":"org"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(body)))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// The dispatch runs in a goroutine, so we need to wait a bit.
	// Use eventually-style check.
	assert.Eventually(t, func() bool {
		events := dispatcher.getEvents()
		return len(events) == 1 && events[0].Type == "push"
	}, 1e9, 1e7)	// 1s timeout, 10ms poll
}
