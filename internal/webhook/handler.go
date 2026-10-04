package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Dispatcher is called to process a parsed webhook event.
type Dispatcher interface {
	Dispatch(ctx context.Context, event Event) DispatchResult
}

// IngestNotifier is told about each delivery after the queue's worker applied it.
type IngestNotifier interface {
	NotifyIngest(event Event, result DispatchResult, ingestedAt time.Time)
}

// Recorder-only dispositions for deliveries that never reach dispatch.
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
// verified delivery is answered once q's journal holds it; q's worker applies
// it. A delivery id already in the journal is answered without recording it twice.
func Handler(secret string, q *Queue) http.HandlerFunc {
	recorder := q.recorder
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

		// Fail closed: without a configured secret we cannot verify that a webhook
		// came from GitHub, and an unauthenticated endpoint
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

		deliveryID := r.Header.Get("X-GitHub-Delivery")
		if deliveryID == "" {
			deliveryID = localDeliveryID()
		}
		event := ParseEvent(eventType, body)
		event.DeliveryID = deliveryID

		// Recorded under its own context: a sender that hangs up mid-write must not leave a half-kept promise.
		fresh, err := q.enqueue(context.WithoutCancel(r.Context()), Stored{
			DeliveryID: deliveryID, EventType: eventType, Body: body, ReceivedAt: receivedAt,
		})
		result := DispatchResult{Event: event.Type, Action: event.Action, Repo: event.RepoFullName()}
		switch {
		case err != nil:
			slog.Error("webhook: recording a delivery failed; GitHub is told so it can be sent again", "type", eventType, "delivery", deliveryID, "error", err)
			result.Disposition, result.Detail = DispError, "recording the delivery failed"
			if recorder != nil {
				recorder.RecordDelivery(event, result, receivedAt, time.Since(receivedAt))
			}
		case !fresh:
			result.Disposition, result.Detail = DispDuplicate, "delivery id already recorded"
			if recorder != nil {
				recorder.RecordDelivery(event, result, receivedAt, time.Since(receivedAt))
			}
		default:
			result.Disposition = DispQueued
		}
		writeResult(w, result)
	}
}

// localDeliveryID names a delivery that arrived without X-GitHub-Delivery, so it is never taken for a repeat.
func localDeliveryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("webhook: read random: " + err.Error())
	}
	return "local-" + hex.EncodeToString(b[:])
}

// writeResult serializes the dispatch outcome as the HTTP response. The status
// distinguishes "applied" () from a no-op () and an internal error ();
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
