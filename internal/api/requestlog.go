package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// Request dispositions recorded for the dashboard's "Requests" view.
const (
	DispHit         = "hit"         // served from a fresh cache, no upstream call
	DispMiss        = "miss"        // cache miss -> fetched from GitHub, then cached
	DispPassthrough = "passthrough" // forwarded to GitHub uncached (not a cached endpoint)
	DispError       = "error"       // the cache lookup/fetch failed
)

// requestEvent is one recorded data-API request.
type requestEvent struct {
	Actor       string `json:"actor"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	At          string `json:"at"` // RFC3339
}

// requestLog is an in-memory, bounded record of recent data-API requests plus
// per-disposition counters, so the dashboard can show traffic hitting the cache
// (hit/miss) vs. forwarded uncached (passthrough). It is deliberately NOT
// persisted: request traffic is high-volume and this is a live operational view,
// not an audit log (unlike webhook_deliveries). It resets on restart.
type requestLog struct {
	mu     sync.Mutex
	total  int64
	byDisp map[string]int64
	recent []requestEvent // newest last; capped at requestLogRecentCap
}

const requestLogRecentCap = 500

func newRequestLog() *requestLog {
	return &requestLog{byDisp: make(map[string]int64)}
}

func (l *requestLog) record(actorKey, method, path, disposition string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	l.byDisp[disposition]++
	l.recent = append(l.recent, requestEvent{
		Actor:       actorKey,
		Method:      method,
		Path:        path,
		Disposition: disposition,
		At:          time.Now().UTC().Format(time.RFC3339),
	})
	if len(l.recent) > requestLogRecentCap {
		l.recent = l.recent[len(l.recent)-requestLogRecentCap:]
	}
}

// requestLogSnapshot is the dashboard payload: totals + recent requests (newest first).
type requestLogSnapshot struct {
	Total         int64            `json:"total"`
	ByDisposition map[string]int64 `json:"by_disposition"`
	Recent        []requestEvent   `json:"recent"`
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
	return requestLogSnapshot{Total: l.total, ByDisposition: byDisp, Recent: recent}
}

// recordPassthrough wraps the GitHub reverse proxy so every request it serves is
// recorded as a passthrough. Used both as the router's NotFound/MethodNotAllowed
// fallback and as the GraphQL handler's forward target, so each proxied request
// is counted exactly once regardless of entry path.
func recordPassthrough(next http.Handler, log *requestLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(callerLabel(r), r.Method, r.URL.Path, DispPassthrough)
		next.ServeHTTP(w, r)
	})
}

// callerLabel derives a best-effort, display-only cache-partition label for a
// request. It never makes a network call: it uses the actor already resolved by
// requireAuth when present (the cached-endpoint path), else the App id from an
// X-Mirror-Identity assertion (decoded WITHOUT verifying — a forged header only
// mislabels a metric row, never a security boundary), else a short token
// fingerprint, else "anonymous".
func callerLabel(r *http.Request) string {
	if a := actor.FromContext(r.Context()); a != "" {
		return a
	}
	if jwt := r.Header.Get("X-Mirror-Identity"); jwt != "" {
		if iss := jwtIssuer(jwt); iss != "" {
			return "app:" + iss
		}
		return "app:?"
	}
	if tok := bearerToken(r); tok != "" {
		fp := ghclient.Fingerprint(tok)
		if len(fp) > 12 {
			fp = fp[:12]
		}
		return "token:" + fp
	}
	return "anonymous"
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
