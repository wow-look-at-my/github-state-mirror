package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// dispatchTimeout bounds how long a webhook dispatch may run, so a stuck store
const dispatchTimeout = 30 * time.Second

// Dispatcher is called to process a parsed webhook event. It returns a
type Dispatcher interface {
	Dispatch(ctx context.Context, event Event) DispatchResult
}

// IngestNotifier is told about a delivery AFTER the synchronous dispatch has
type IngestNotifier interface {
	NotifyIngest(event Event, result DispatchResult, ingestedAt time.Time)
}

// Recorder-only dispositions for deliveries that never reach dispatch. They
// feed the timeline chart (every delivery attempt is recorded, whatever its
// fate — a gap on the chart is a bug); they are never returned to GitHub,
// whose responses stay the plain http.Error texts.
const (
	// DispUnverified marks a delivery whose authenticity could not be
	DispUnverified = "unverified"
	// DispUnparseable marks a VERIFIED delivery missing the event-type
	DispUnparseable = "unparseable"
	// DispRejected marks a request refused before verification could even be
	DispRejected = "rejected"
)

// DeliveryRecorder observes EVERY delivery attempt — verified deliveries
type DeliveryRecorder interface {
	RecordDelivery(event Event, result DispatchResult, receivedAt time.Time, duration time.Duration)
}

// Handler returns an http.Handler that receives GitHub webhook POSTs. A
// non-nil recorder is told about each verified delivery with its measured
// handling duration; any non-nil notifiers are invoked (non-blocking) after
// each dispatch with the dispatch outcome — the variadic keeps notifier-less
// call sites unchanged.
func Handler(secret string, dispatcher Dispatcher, recorder DeliveryRecorder, notifiers ...IngestNotifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The handling clock starts at receipt, so EVERY outcome — rejected,
		receivedAt := time.Now()
		reject := func(disposition, detail string) {
			if recorder == nil {
				return
			}
			recorder.RecordDelivery(
				Event{Type: r.Header.Get("X-GitHub-Event"), DeliveryID: r.Header.Get("X-GitHub-Delivery")},
				DispatchResult{Disposition: disposition, Detail: detail},
				receivedAt, time.Since(receivedAt),
			)
		}

		if r.Method != http.MethodPost {
			reject(DispRejected, "method not allowed")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			reject(DispRejected, "unreadable body")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Fail closed: without a configured secret we cannot verify that a
		// webhook actually came from GitHub, and an unauthenticated endpoint
		if secret == "" {
			slog.Error("webhook rejected: WEBHOOK_SECRET is not set")
			reject(DispUnverified, "webhook secret not configured")
			http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
			return
		}
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifySignature(secret, sig, body) {
			slog.Warn("webhook signature verification failed")
			reject(DispUnverified, "signature verification failed")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")
		if eventType == "" {
			// Verified (GitHub-signed) but undispatched: no event type.
			if recorder != nil {
				recorder.RecordDelivery(
					Event{DeliveryID: r.Header.Get("X-GitHub-Delivery")},
					DispatchResult{Disposition: DispUnparseable, Detail: "missing event type"},
					receivedAt, time.Since(receivedAt),
				)
			}
			http.Error(w, "missing event type", http.StatusBadRequest)
			return
		}

		event := ParseEvent(eventType, body)
		event.DeliveryID = r.Header.Get("X-GitHub-Delivery")

		// Dispatch synchronously so the response reflects the real outcome (the
		ctx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
		defer cancel()
		result := dispatcher.Dispatch(ctx, event)

		if recorder != nil {
			recorder.RecordDelivery(event, result, receivedAt, time.Since(receivedAt))
		}

		// Ingest is done — hand the outcome to any subscriber notifier. The
		ingestedAt := time.Now()
		for _, n := range notifiers {
			if n != nil {
				n.NotifyIngest(event, result, ingestedAt)
			}
		}

		writeResult(w, result)
	}
}

// writeResult serializes the dispatch outcome as the HTTP response. The status
// distinguishes "applied" (200) from a no-op (202) and an internal error (500);
// the body and headers carry the detail so it shows up in GitHub's delivery UI.
func writeResult(w http.ResponseWriter, result DispatchResult) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GSM-Disposition", result.Disposition)
	w.WriteHeader(result.StatusCode())
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Warn("webhook: encode response failed", "error", err)
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
