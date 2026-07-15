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
// operation can't hold the connection open indefinitely. The cache writes are
// small idempotent upserts that complete well within GitHub's delivery deadline,
// so dispatch runs synchronously (see ServeHTTP) and this is only a safety net.
const dispatchTimeout = 30 * time.Second

// Dispatcher is called to process a parsed webhook event. It returns a
// DispatchResult describing what it did, which the handler reports back to
// GitHub so the delivery record reflects whether the cache was updated.
type Dispatcher interface {
	Dispatch(ctx context.Context, event Event) DispatchResult
}

// IngestNotifier is told about a delivery AFTER the synchronous dispatch has
// applied it, so subscriber notifications (internal/notify) can fan out
// post-ingest. Implementations MUST return immediately (enqueue/spawn): the
// GitHub webhook response never waits on subscriber POSTs. The notifier is
// optional — omitted or nil keeps the feature inert.
type IngestNotifier interface {
	NotifyIngest(event Event, result DispatchResult, ingestedAt time.Time)
}

// DeliveryRecorder observes every VERIFIED delivery after its synchronous
// dispatch completes, with the real measured handling duration (receipt after
// signature verification → dispatch complete — parse included, never faked to
// an instant). It feeds the dashboard's timeline chart (internal/reqtimeline).
// Implementations must be fast and non-blocking (an in-memory append); nil
// keeps the feature inert.
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

		// The handling clock starts here — after verification, before parse —
		// so the recorded duration is the delivery's REAL parse+dispatch time,
		// however short (webhook handling is fast; the timeline renders it as
		// whatever it truly measured, never an artificial instant).
		receivedAt := time.Now()

		event := ParseEvent(eventType, body)
		event.DeliveryID = r.Header.Get("X-GitHub-Delivery")

		// Dispatch synchronously so the response reflects the real outcome (the
		// cache writes are fast, idempotent upserts). Bound it so a stuck store
		// op can't pin the connection; a retried delivery re-applies cleanly.
		ctx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
		defer cancel()
		result := dispatcher.Dispatch(ctx, event)

		if recorder != nil {
			recorder.RecordDelivery(event, result, receivedAt, time.Since(receivedAt))
		}

		// Ingest is done — hand the outcome to any subscriber notifier. The
		// call is non-blocking by contract, so the response to GitHub is
		// never held up by subscriber endpoints.
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
