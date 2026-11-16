// The embedded api-mirror engine: the dispatch that sends it the routes its
// spec declares.

package api

import (
	"net/http"

	apimirror "github.com/wow-look-at-my/api-mirror/engine"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// Engine is what the router needs from internal/engine. A nil Engine serves nothing.
type Engine interface {
	http.Handler
	Declares(method, escapedPath string) bool
	SetRequestSink(func(apimirror.Request))
}

// engineDispatch sends a request the engine declares to the engine, behind authed, and every other request on.
// It runs after gheCompat, so the Enterprise spelling reaches the engine canonical. The engine never sees its
// own dashboard, notify or events paths, because Declares answers only for spec routes.
func engineDispatch(eng Engine, authed func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if eng == nil {
			return next
		}
		served := authed(eng)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if eng.Declares(r.Method, r.URL.EscapedPath()) {
				served.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// recordEngine puts one engine answer in the request log and on the Timeline. The engine's
// disposition words are the log's own words, plus denied and refusal for a reveal or absorbed refusal.
// A forwarded delivery is skipped: the webhook worker already put it on the Timeline, and the forwarder logs a refusal.
func (l *requestLog) recordEngine(r apimirror.Request) {
	if r.Disposition == apimirror.DispositionDelivery {
		return
	}
	who := callerIdent{Key: r.Principal}
	disp := string(r.Disposition)
	l.recordFull(who, r.Method, r.Path, disp, r.Status, r.Reason, "")
	l.timeline.RecordRequest(r.At, r.Duration, r.Method, normalizeRoute(r.Path), r.Status, disp, who.Key, who.Name)
}

// EngineObserver charts every outbound request the engine sends. Those charts are under the labels the API layer's
// own client uses. Reports each answer's X-RateLimit-* headers to the rate meter, as the API layer's own fetches
// do.
func EngineObserver(tl *reqtimeline.Recorder, meter *ratemeter.Store) func(apimirror.Exchange) {
	return func(x apimirror.Exchange) {
		if x.Header != nil {
			meter.Observe(x.Principal, "", &http.Response{Header: x.Header})
		}
		disp := dispUpstream
		switch {
		case x.Err != nil || x.Status == 0:
			disp = DispError
		case x.Lane == "probe":
			disp = dispProbe
		case x.Lane != "fetch" && x.Lane != "passthrough":
			disp = dispInternal
		}
		tl.RecordRequest(x.Started, x.Duration, x.Method, normalizeRoute(x.Path), x.Status, disp, x.Principal, "")
	}
}
