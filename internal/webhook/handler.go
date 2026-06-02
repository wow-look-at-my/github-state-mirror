package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// dispatchTimeout bounds how long an asynchronous webhook dispatch may run,
// so a slow or stuck refresh can't leak goroutines indefinitely.
const dispatchTimeout = 30 * time.Second

// Dispatcher is called to process a parsed webhook event.
type Dispatcher interface {
	Dispatch(ctx context.Context, event Event)
}

// Handler returns an http.Handler that receives GitHub webhook POSTs.
func Handler(secret string, dispatcher Dispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Fail closed: without a configured secret we cannot verify that a
		// webhook actually came from GitHub, and an unauthenticated endpoint
		// that mutates the cache would let anyone inject data into other
		// callers' partitions. Refuse rather than trust the payload.
		if secret == "" {
			slog.Error("webhook rejected: WEBHOOK_SECRET is not set")
			http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
			return
		}
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifySignature(secret, sig, body) {
			slog.Warn("webhook signature verification failed")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")
		if eventType == "" {
			http.Error(w, "missing event type", http.StatusBadRequest)
			return
		}

		event := ParseEvent(eventType, body)

		// Respond 200 immediately, dispatch asynchronously.
		// Use a detached, time-bounded context since the request context will
		// be canceled once we return.
		w.WriteHeader(http.StatusOK)

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
			defer cancel()
			dispatcher.Dispatch(ctx, event)
		}()
	}
}

func verifySignature(secret, signature string, body []byte) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}
