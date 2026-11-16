// The applier that gives the engine each delivery inside the worker's own apply.

package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// forwardTimeout bounds the engine's apply of one delivery.
const forwardTimeout = 30 * time.Second

type eventsHandler interface {
	http.Handler
	EventsPath() string
}

// applier applies a delivery to this service, then to the engine.
type applier struct {
	next   webhook.Dispatcher
	eng    http.Handler
	path   string
	secret string
}

func newApplier(next webhook.Dispatcher, eng eventsHandler, secret string) *applier {
	return &applier{next: next, eng: eng, path: eng.EventsPath(), secret: secret}
}

// Dispatch reports the worse of the outcomes. An engine refusal is an error,
// so the worker tries the delivery again and the Webhooks tab shows it. A
// delivery this service ignores reports what the engine did with it, because
// a type only the engine handles must not read as untracked.
func (a *applier) Dispatch(ctx context.Context, event webhook.Event) webhook.DispatchResult {
	result := a.next.Dispatch(ctx, event)
	disp, err := a.forward(ctx, event)
	if err != nil {
		slog.Error("engine: a delivery was refused; its routes keep their last state until the TTL",
			"delivery", event.DeliveryID, "type", event.Type, "error", err)
		if result.Disposition != webhook.DispError {
			result.Disposition = webhook.DispError
			result.Detail = "engine: " + err.Error()
		}
		return result
	}
	if result.Disposition == webhook.DispIgnored {
		switch disp {
		case webhook.DispApplied, webhook.DispInvalidated, webhook.DispSuperseded:
			result.Disposition = disp
			result.Detail = "engine: " + disp
		}
	}
	return result
}

// forward posts one delivery in process, signed as GitHub signs it, and
// returns the engine's disposition.
func (a *applier) forward(ctx context.Context, event webhook.Event) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.path, bytes.NewReader(event.Raw))
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write(event.Raw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event.Type)
	req.Header.Set("X-GitHub-Delivery", event.DeliveryID)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := &statusWriter{header: http.Header{}}
	a.eng.ServeHTTP(w, req)
	if w.code >= 300 {
		return "", fmt.Errorf("status %d: %s", w.code, w.body.String())
	}
	return strings.TrimSpace(w.body.String()), nil
}

// statusWriter keeps the status and a capped body of an in-process answer.
type statusWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *statusWriter) Header() http.Header { return w.header }

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if room := 512 - w.body.Len(); room > 0 {
		w.body.Write(b[:min(len(b), room)])
	}
	return len(b), nil
}
