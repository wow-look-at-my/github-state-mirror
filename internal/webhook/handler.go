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
)

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

		if secret != "" {
			sig := r.Header.Get("X-Hub-Signature-256")
			if !verifySignature(secret, sig, body) {
				slog.Warn("webhook signature verification failed")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		eventType := r.Header.Get("X-GitHub-Event")
		if eventType == "" {
			http.Error(w, "missing event type", http.StatusBadRequest)
			return
		}

		event := ParseEvent(eventType, body)

		// Respond 200 immediately, dispatch asynchronously.
		// Use a detached context since the request context will be canceled.
		w.WriteHeader(http.StatusOK)

		go dispatcher.Dispatch(context.Background(), event)
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
