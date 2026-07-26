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

// dispositionHintKey lets a handler that forwards to the passthrough proxy
// override the recorded disposition (e.g. the GraphQL route marking a
// forwarded mutation as a write).
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
	// ActorName is the principal's VERIFIED display name (user login / app
	// slug), captured from the request context at record time. Empty (and
	// omitted) when no verified name is known — notably the unverified
	// X-Mirror-Identity "app:<iss>" label, which must never gain a name.
	ActorName   string `json:"actor_name,omitempty"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	// Status is the upstream HTTP status for a passthrough (so the row shows
	// whether GitHub actually accepted it — 200 vs 401/404/502). 0 when not
	// applicable (e.g. a cache hit makes no upstream call).
	Status int    `json:"status,omitempty"`
	At     string `json:"at"` // RFC3339
}

// requestLog is an in-memory, bounded record of recent data-API requests plus
// per-disposition counters and per-route-shape GROUP counters (requestgroups.go),
// so the dashboard can show traffic hitting the cache (hit/miss) vs. forwarded
// uncached (passthrough) — both as a flat recent list and aggregated by route
// shape. It is deliberately NOT persisted: request traffic is high-volume and
// this is a live operational view, not an audit log (unlike webhook_deliveries).
// It resets on restart.
type requestLog struct {
	mu     sync.Mutex
	total  int64
	byDisp map[string]int64
	groups map[string]*routeGroup // key: method + " " + normalizeRoute(path); bounded (requestgroups.go)
	recent []requestEvent         // newest last; capped at requestLogRecentCap
	// timeline mirrors every observed request onto the dashboard's Timeline
	// chart with its real end-to-end duration (see observeStatus). Nil-safe.
	timeline *reqtimeline.Recorder
}

const requestLogRecentCap = 500

func newRequestLog() *requestLog {
	return &requestLog{byDisp: make(map[string]int64), groups: make(map[string]*routeGroup)}
}

// observe records one served data-API request — into the request log AND,
// timed end-to-end from the router's receipt stamp, into the timeline ring.
// EVERY inbound disposition is charted: hits included (a hit is a request the
// mirror served; concealing it from the chart would be a gap).
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
	l.recordStatus(who, r.Method, r.URL.Path, disposition, status)
	// The router stamps every request (stampRequestStart), so the stamp is
	// always present on served traffic; a direct handler invocation in a unit
	// test without the router is the only stampless path.
	if start, ok := requestStartFrom(r.Context()); ok {
		l.timeline.RecordRequest(start, time.Since(start), r.Method, normalizeRoute(r.URL.Path), status, disposition, who.Key, who.Name)
	}
}

func (l *requestLog) record(who callerIdent, method, path, disposition string) {
	l.recordStatus(who, method, path, disposition, 0)
}

func (l *requestLog) recordStatus(who callerIdent, method, path, disposition string, status int) {
	now := time.Now().UTC()
	route := normalizeRoute(path) // pure; kept outside the critical section
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	l.byDisp[disposition]++
	l.bumpGroupLocked(method, route, path, disposition, now)
	l.recent = append(l.recent, requestEvent{
		Actor:       who.Key,
		ActorName:   who.Name,
		Method:      method,
		Path:        path,
		Disposition: disposition,
		Status:      status,
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
	// DBSizeBytes / DBWALSizeBytes are the SQLite database file's (and its -wal
	// sidecar's) on-disk sizes — the cache's real footprint. Filled by the
	// dashboard handler (which knows DB_PATH), not by snapshot(); 0/omitted
	// when the file is missing or unreadable.
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
func recordPassthrough(next http.Handler, log *requestLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.observeStatus(r, passthroughDisposition(r), sw.status)
	})
}

// statusRecorder wraps an http.ResponseWriter to capture the status code while
// otherwise behaving transparently (including flushing, which the reverse proxy
// relies on to stream responses).
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
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
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// callerIdent identifies a request's caller for display surfaces (request
// log, rate meter): the partition/label Key plus the VERIFIED display Name
// (user login / app slug), empty when none was proven. Display-only — never a
// storage key.
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

// principalNameAttr returns an inline slog attr carrying the principal's
// verified display name ("principal_name") when the context has one, or a
// no-op attr (an empty group, which slog handlers elide) when it doesn't — so
// log sites can append it unconditionally and only named principals gain the
// field.
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
