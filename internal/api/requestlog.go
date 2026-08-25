package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// Request dispositions recorded for the dashboard's "Requests" view.
const (
	DispHit         = "hit"         // served from cached global truth, no upstream call
	DispMiss        = "miss"        // fetched from GitHub with the caller's token, absorbed, then served
	DispPassthrough = "passthrough" // a READ forwarded to GitHub uncached (unknown route / non-default shape)
	DispWrite       = "write"       // a MUTATING method proxied to GitHub (never cacheable by design)
	DispError       = "error"       // the cache lookup/fetch failed
)

// Passthrough REASONS: WHY a read was forwarded to GitHub uncached. The
// disposition alone says a route did not serve from state; the reason says
// whether that is a caching GAP (a shape worth modeling) or the model working
// as designed (a shape deliberately left uncacheable). Without it, reading the
// dashboard's "Top uncached requests" table means going to the source — of
// this repo AND of the calling service — to recover the query shape the log
// threw away, which is exactly the archaeology this vocabulary replaces.
//
// The set is CLOSED and every value is a compile-time constant: reasons are
// group-counter keys, so — like the timeline's lane names — they must never be
// derived from a URL, a header, or anything else a caller controls.
const (
	// PassAccept: a non-default Accept media type (raw/html/diff/patch) — a different response shape entirely.
	PassAccept = "unmodeled-accept"
	// PassQuery: query params the route does not model — the dominant reason in practice; a gap to close, not a verdict.
	PassQuery = "unmodeled-query"
	// PassPath: route matched, but a path segment is outside the model (short sha, non-numeric PR number, cross-fork basehead, ...).
	PassPath = "unmodeled-path"
	// PassUnrouted: no cached route claims this path (chi's NotFound) — where every new caching candidate starts.
	PassUnrouted = "unrouted"
	// PassMethod: the path has a cached route, but not for this method (chi's MethodNotAllowed).
	PassMethod = "unrouted-method"
	// PassIdentity: a self-verifying App-JWT route whose bearer did not verify or is absent.
	PassIdentity = "unverified-identity"
	// PassResponse: the request was modeled but the response was not (unexpected status, oversized body, ...) — costs an upstream round trip.
	PassResponse = "unmodeled-response"
	// PassGraphQL: a GraphQL query other than the locked org-repos one.
	PassGraphQL = "graphql-forward"
)

// passthroughReasonKey carries the reason from the declining handler to the central recorder.
type passthroughReasonKey struct{}

func withPassthroughReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, passthroughReasonKey{}, reason)
}

func passthroughReasonFrom(ctx context.Context) string {
	if v, ok := ctx.Value(passthroughReasonKey{}).(string); ok {
		return v
	}
	return ""
}

// passthrough forwards a declined read to GitHub, tagged with why. Every cached route's bail-out goes through here, never the proxy directly.
func (h *handlers) passthrough(w http.ResponseWriter, r *http.Request, reason string) {
	h.ghProxy.ServeHTTP(w, r.WithContext(withPassthroughReason(r.Context(), reason)))
}

// taggedProxy is h.passthrough's equivalent for chi's NotFound/MethodNotAllowed fallback, where no handler ever saw the request.
func taggedProxy(next http.Handler, reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withPassthroughReason(r.Context(), reason)))
	}
}

// shapeReason checks Accept, then path, then query, in a fixed order, so a request violating several reports one stable reason.
func shapeReason(r *http.Request, modeledPath bool) string {
	switch {
	case !acceptsDefaultJSON(r):
		return PassAccept
	case !modeledPath:
		return PassPath
	default:
		return PassQuery
	}
}

// dispositionHintKey lets a handler override the recorded disposition (e.g. GraphQL marking a forwarded mutation as a write).
type dispositionHintKey struct{}

func withDispositionHint(ctx context.Context, disp string) context.Context {
	return context.WithValue(ctx, dispositionHintKey{}, disp)
}

func dispositionHint(ctx context.Context) string {
	if v, ok := ctx.Value(dispositionHintKey{}).(string); ok {
		return v
	}
	return ""
}

// passthroughDisposition classifies a proxied request: mutating methods are
// writes (forwarded because GitHub is the only writer, not because a read
// failed to cache); reads keep the passthrough label. A context hint wins.
func passthroughDisposition(r *http.Request) string {
	if hint := dispositionHint(r.Context()); hint != "" {
		return hint
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return DispPassthrough
	default:
		return DispWrite
	}
}

// requestEvent is one recorded data-API request.
type requestEvent struct {
	Actor string `json:"actor"`
	// ActorName is the principal's verified display name; empty for an unverified X-Mirror-Identity "app:<iss>" label.
	ActorName   string `json:"actor_name,omitempty"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	// Status is the upstream HTTP status for a passthrough; 0 when no upstream call was made (e.g. a cache hit).
	Status int `json:"status,omitempty"`
	// Reason is why a passthrough was forwarded uncached (one of the Pass* vocabulary above); empty for every other disposition.
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"` // RFC3339
}

// requestLog is an in-memory, bounded record of recent requests plus
// per-disposition and per-route-shape counters (requestgroups.go).
// see docs/dashboard/dashboard.md
type requestLog struct {
	mu     sync.Mutex
	total  int64
	byDisp map[string]int64
	groups map[string]*routeGroup // key: method + " " + normalizeRoute(path); bounded (requestgroups.go)
	recent []requestEvent         // newest last; capped at requestLogRecentCap
	// timeline mirrors every request onto the Timeline chart with its real duration; nil-safe.
	timeline *reqtimeline.Recorder
}

const requestLogRecentCap = 500

func newRequestLog() *requestLog {
	return &requestLog{byDisp: make(map[string]int64), groups: make(map[string]*routeGroup)}
}

// observe records the request into the log and, timed end-to-end, onto the Timeline (every disposition, hits included).
func (l *requestLog) observe(r *http.Request, disposition string) {
	l.observeStatus(r, disposition, 0)
}

func (l *requestLog) observeStatus(r *http.Request, disposition string, status int) {
	l.observeAs(r, callerLabel(r), disposition, status)
}

// observeAs is observeStatus with an explicit caller identity — for the
// self-verifying app-JWT routes, whose verified app:<id>+slug identity
// callerLabel cannot derive.
func (l *requestLog) observeAs(r *http.Request, who callerIdent, disposition string, status int) {
	l.recordFull(who, r.Method, r.URL.Path, disposition, status, passthroughReasonFrom(r.Context()), queryShape(r.URL.Query()))
	// Only a direct unit-test call bypassing the router lacks this stamp.
	if start, ok := requestStartFrom(r.Context()); ok {
		l.timeline.RecordRequest(start, time.Since(start), r.Method, normalizeRoute(r.URL.Path), status, disposition, who.Key, who.Name)
	}
}

func (l *requestLog) record(who callerIdent, method, path, disposition string) {
	l.recordFull(who, method, path, disposition, 0, "", "")
}

func (l *requestLog) recordStatus(who callerIdent, method, path, disposition string, status int) {
	l.recordFull(who, method, path, disposition, status, "", "")
}

// recordFull is the single recording path: totals, the route-shape group (with
// its passthrough-reason tally), and the recent ring.
func (l *requestLog) recordFull(who callerIdent, method, path, disposition string, status int, reason, qshape string) {
	if disposition != DispPassthrough {
		// Only a passthrough has a reason to explain; clear any stray hint on other dispositions.
		reason, qshape = "", ""
	}
	now := time.Now().UTC()
	route := normalizeRoute(path) // pure; kept outside the critical section
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	l.byDisp[disposition]++
	l.bumpGroupLocked(method, route, path, disposition, reason, qshape, now)
	l.recent = append(l.recent, requestEvent{
		Actor:       who.Key,
		ActorName:   who.Name,
		Method:      method,
		Path:        path,
		Disposition: disposition,
		Status:      status,
		Reason:      reason,
		At:          now.Format(time.RFC3339),
	})
	if len(l.recent) > requestLogRecentCap {
		l.recent = l.recent[len(l.recent)-requestLogRecentCap:]
	}
}

// requestLogSnapshot is the dashboard payload: totals + route-shape groups
// (total desc, capped) + recent requests (newest first).
type requestLogSnapshot struct {
	Total         int64                  `json:"total"`
	ByDisposition map[string]int64       `json:"by_disposition"`
	Groups        []requestGroupSnapshot `json:"groups"`
	Recent        []requestEvent         `json:"recent"`
	// The DB file's (and its -wal sidecar's) on-disk sizes, filled by the
	// dashboard handler (which knows DB_PATH) — 0/omitted if unreadable.
	DBSizeBytes    int64 `json:"db_size_bytes,omitempty"`
	DBWALSizeBytes int64 `json:"db_wal_size_bytes,omitempty"`
}

func (l *requestLog) snapshot(limit int) requestLogSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	byDisp := make(map[string]int64, len(l.byDisp))
	for k, v := range l.byDisp {
		byDisp[k] = v
	}
	n := len(l.recent)
	if limit > 0 && limit < n {
		n = limit
	}
	recent := make([]requestEvent, 0, n)
	for i := len(l.recent) - 1; i >= 0 && len(recent) < n; i-- {
		recent = append(recent, l.recent[i])
	}
	return requestLogSnapshot{
		Total:         l.total,
		ByDisposition: byDisp,
		Groups:        l.groupSnapshotsLocked(requestGroupsSnapshotCap),
		Recent:        recent,
	}
}

// recordPassthrough wraps the GitHub reverse proxy so every request it serves is
// recorded as a passthrough — with the upstream HTTP status GitHub returned, so
// the dashboard shows whether the forwarded call actually succeeded. Used both as
// the router's NotFound/MethodNotAllowed fallback and as the GraphQL handler's
// forward target, so each proxied request is counted exactly once regardless of
// entry path. observeStatus also times it end-to-end (upstream round-trip plus
// response streaming) into the timeline ring.
func recordPassthrough(next http.Handler, log *requestLog, shapes *shapeStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// Sample the response shape for the brief, at most once per route shape per shapeResampleAfter.
		route := normalizeRoute(r.URL.Path)
		if shapes.wantsBody(r.Method, route) {
			sw.capture = make([]byte, 0, 8<<10)
		}
		next.ServeHTTP(sw, r)
		log.observeStatus(r, passthroughDisposition(r), sw.status)
		shapes.observeRequest(r, route, sw.status, sw.Header().Get("Content-Type"), sw.Header().Get("Content-Encoding"), sw.capturedBody())
	})
}

// observeRequest records one passthrough's shape. body is nil unless this
// request was selected for sampling; it is reduced to a skeleton and dropped.
// contentEncoding is the upstream response's own Content-Encoding (e.g.
// "gzip") — body is the WIRE bytes, not decoded, since the passthrough proxy
// relays a caller's Accept-Encoding to GitHub unchanged (see decodeSample).
func (s *shapeStore) observeRequest(r *http.Request, route string, status int, contentType, contentEncoding string, body []byte) {
	if s == nil {
		return
	}
	who := callerLabel(r)
	name := who.Name
	if name == "" {
		name = actor.Short(who.Key)
	}
	q := r.URL.Query()
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, clampRoute(k))
	}
	s.observe(observation{
		Method: r.Method, Route: route, Path: r.URL.Path,
		QueryNames: names, Accept: r.Header.Get("Accept"), Caller: name,
		Status: status, ContentType: contentType, ContentEncoding: contentEncoding, Body: body,
	})
}

// statusRecorder wraps an http.ResponseWriter to capture the status code while
// otherwise behaving transparently (including flushing, which the reverse proxy
// relies on to stream responses).
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
	// capture, when non-nil, buffers a response prefix for shape sampling.
	// Past shapeMaxSampleBytes it is abandoned, not truncated — a truncated body parses to nothing.
	capture  []byte
	overflow bool
}

func (s *statusRecorder) capturedBody() []byte {
	if s.overflow {
		return nil
	}
	return s.capture
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true // an implicit 200 when WriteHeader was never called
	if s.capture != nil && !s.overflow {
		if len(s.capture)+len(b) > shapeMaxSampleBytes {
			s.capture, s.overflow = nil, true
		} else {
			s.capture = append(s.capture, b...)
		}
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// callerIdent is a caller's display Key plus its VERIFIED display Name (may be empty).
// Display-only — never a storage key.
type callerIdent struct {
	Key  string
	Name string
}

// callerLabel derives a best-effort, display-only caller identity for a
// request. It never makes a network call: it uses the actor (and its verified
// display name) already resolved by requireAuth when present (the
// cached-endpoint path), else the App id from an X-Mirror-Identity assertion
// (decoded WITHOUT verifying — a forged header only mislabels a metric row,
// never a security boundary; deliberately NO name, names require
// verification), else a short token fingerprint, else "anonymous".
func callerLabel(r *http.Request) callerIdent {
	ctx := r.Context()
	if a := actor.FromContext(ctx); a != "" {
		return callerIdent{Key: a, Name: actor.NameFromContext(ctx)}
	}
	if jwt := r.Header.Get("X-Mirror-Identity"); jwt != "" {
		if iss := jwtIssuer(jwt); iss != "" {
			return callerIdent{Key: "app:" + iss}
		}
		return callerIdent{Key: "app:?"}
	}
	if tok := bearerToken(r); tok != "" {
		fp := ghclient.Fingerprint(tok)
		if len(fp) > 12 {
			fp = fp[:12]
		}
		return callerIdent{Key: "token:" + fp}
	}
	return callerIdent{Key: "anonymous"}
}

// principalNameAttr returns a "principal_name" slog attr when known, else a no-op empty group — so callers can append it unconditionally.
func principalNameAttr(ctx context.Context) slog.Attr {
	if name := actor.NameFromContext(ctx); name != "" {
		return slog.Group("", slog.String("principal_name", name))
	}
	return slog.Group("")
}

// jwtIssuer extracts the `iss` claim from a JWT WITHOUT verifying its signature.
// Display-only (see callerLabel); returns "" if the token can't be parsed.
func jwtIssuer(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss json.RawMessage `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.Trim(string(claims.Iss), `"`)
}
