package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
)

// Every response the mirror answers itself carries the caller's X-RateLimit-* headers, the way GitHub's own answers do.

// principalSlotKey keys the per-request slot withPrincipal fills, so the header writer, which wraps the router.
type principalSlotKey struct{}

// withPrincipal sets the request's principal on ctx and on the request's
// rate-limit header slot, when the router installed one.
func withPrincipal(ctx context.Context, key string) context.Context {
	if slot, ok := ctx.Value(principalSlotKey{}).(*atomic.Pointer[string]); ok {
		slot.Store(&key)
	}
	return actor.WithActor(ctx, key)
}

// rateLimitHeaders wraps every response with rateLimitWriter.
func rateLimitHeaders(meter *ratemeter.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slot := &atomic.Pointer[string]{}
			r = r.WithContext(context.WithValue(r.Context(), principalSlotKey{}, slot))
			next.ServeHTTP(&rateLimitWriter{ResponseWriter: w, meter: meter, req: r, slot: slot}, r)
		})
	}
}

// rateLimitWriter adds the caller's X-RateLimit-* headers when the response is
// committed without them.
type rateLimitWriter struct {
	http.ResponseWriter
	meter   *ratemeter.Store
	req     *http.Request
	slot    *atomic.Pointer[string]
	written bool
}

func (w *rateLimitWriter) WriteHeader(code int) {
	if !w.written {
		w.written = true
		w.fill()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *rateLimitWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *rateLimitWriter) Flush() {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *rateLimitWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *rateLimitWriter) fill() {
	h := w.Header()
	if h.Get("X-RateLimit-Limit") != "" {
		return
	}
	identity := w.identity()
	if identity == "" {
		return
	}
	resource := rateResourceFor(w.req.URL.Path)
	for _, o := range w.meter.ObservationsFor(identity) {
		if o.Resource != resource {
			continue
		}
		h.Set("X-RateLimit-Limit", strconv.Itoa(o.Limit))
		h.Set("X-RateLimit-Remaining", strconv.Itoa(o.Remaining))
		h.Set("X-RateLimit-Used", strconv.Itoa(o.Used))
		h.Set("X-RateLimit-Reset", strconv.FormatInt(o.Reset, 10))
		h.Set("X-RateLimit-Resource", o.Resource)
		return
	}
}

// identity is the key the meter recorded this caller's readings under: the
// principal a handler resolved, else callerLabel's credential-derived label.
func (w *rateLimitWriter) identity() string {
	if p := w.slot.Load(); p != nil {
		return *p
	}
	who := callerLabel(w.req).Key
	if who == "anonymous" {
		return ""
	}
	return who
}

// rateResourceFor names the GitHub rate-limit bucket a request path counts
// against, as GitHub reports it in X-RateLimit-Resource.
func rateResourceFor(path string) string {
	switch {
	case path == "/graphql":
		return "graphql"
	case path == "/search/code":
		return "code_search"
	case strings.HasPrefix(path, "/search/"):
		return "search"
	}
	return "core"
}
