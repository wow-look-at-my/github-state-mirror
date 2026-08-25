package api

import (
	"github.com/wow-look-at-my/go-containers/set"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Request grouping for the dashboard's Requests tab: a cumulative
// per-(method, route-shape) counter, live on the requestLog.
// see docs/dashboard/dashboard.md

const (
	// A route count near this cap means garbage paths, not real traffic.
	requestGroupsCap = 1000
	// requestGroupsSnapshotCap caps the groups in one /api/requests payload.
	requestGroupsSnapshotCap = 100
	// routeMaxLen clamps a pathological route string so group keys stay small.
	routeMaxLen = 200
)

// routeGroup is the cumulative tally for one (method, route shape).
type routeGroup struct {
	method string
	route  string
	total  int64
	byDisp map[string]int64
	// byReason splits PASSTHROUGH count by why (see docs/dashboard/dashboard.md).
	byReason map[string]int64
	sample   string // one recent raw path, for identifying the shape
	// passQuery is one recent passthrough's sorted parameter NAMES, never
	// values. see docs/cache/three-tier-contract.md
	passQuery string
	// debounced / upstreamSaved: passthrough coalescing counters, kept
	// separate so a held-but-unsaved route is visible. see docs/cache/three-tier-contract.md
	debounced     int64
	upstreamSaved int64
	lastSeen      time.Time
}

// requestGroupSnapshot is one group in the /api/requests payload.
type requestGroupSnapshot struct {
	Key           string           `json:"key"` // method + " " + route
	Method        string           `json:"method"`
	Route         string           `json:"route"`
	Total         int64            `json:"total"`
	Hit           int64            `json:"hit"`
	Miss          int64            `json:"miss"`
	Passthrough   int64            `json:"passthrough"`
	Write         int64            `json:"write"`
	Error         int64            `json:"error"`
	ByReason      map[string]int64 `json:"by_reason,omitempty"`  // omitted if never passed through
	PassQuery     string           `json:"pass_query,omitempty"` // sorted parameter NAMES, never values
	Debounced     int64            `json:"debounced,omitempty"`
	UpstreamSaved int64            `json:"upstream_saved,omitempty"`
	Sample        string           `json:"sample"`
	LastSeen      string           `json:"last_seen"` // RFC3339
}

// addDebounced may open a group before the recording wrapper counts a
// disposition into it; nil-receiver-safe.
func (l *requestLog) addDebounced(method, route string, served, saved int64) {
	if l == nil || (served == 0 && saved == 0) {
		return
	}
	key := method + " " + route
	l.mu.Lock()
	defer l.mu.Unlock()
	g := l.groups[key]
	if g == nil {
		if len(l.groups) >= requestGroupsCap {
			return
		}
		g = &routeGroup{method: method, route: route, byDisp: make(map[string]int64, 4)}
		l.groups[key] = g
	}
	g.debounced += served
	g.upstreamSaved += saved
}

// bumpGroupLocked records one request into its group. reason/queryShape are
// set only for passthroughs. Caller holds l.mu.
func (l *requestLog) bumpGroupLocked(method, route, rawPath, disposition, reason, queryShape string, now time.Time) {
	key := method + " " + route
	g := l.groups[key]
	if g == nil {
		if len(l.groups) >= requestGroupsCap {
			return // full: drop new shapes, keep counting known ones (see cap doc)
		}
		g = &routeGroup{method: method, route: route, byDisp: make(map[string]int64, 4)}
		l.groups[key] = g
	}
	g.total++
	g.byDisp[disposition]++
	if reason != "" {
		if g.byReason == nil {
			g.byReason = make(map[string]int64, 2)
		}
		g.byReason[reason]++
	}
	if disposition == DispPassthrough {
		// Prefer a non-empty shape so a bare-path hit can't erase evidence.
		if queryShape != "" || g.passQuery == "" {
			g.passQuery = queryShape
		}
	}
	g.sample = rawPath
	g.lastSeen = now
}

// queryShape renders sorted query parameter NAMES, never values (a value can
// carry a credential); "page,per_page,status".
func queryShape(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names)
	return clampRoute(strings.Join(names, ","))
}

// groupSnapshotsLocked returns the groups sorted by total desc (key asc on
// ties, for determinism), capped at max. Caller holds l.mu.
func (l *requestLog) groupSnapshotsLocked(max int) []requestGroupSnapshot {
	gs := make([]requestGroupSnapshot, 0, len(l.groups))
	for key, g := range l.groups {
		var byReason map[string]int64
		if len(g.byReason) > 0 {
			byReason = make(map[string]int64, len(g.byReason))
			for k, v := range g.byReason {
				byReason[k] = v
			}
		}
		gs = append(gs, requestGroupSnapshot{
			Key:           key,
			Method:        g.method,
			Route:         g.route,
			Total:         g.total,
			Hit:           g.byDisp[DispHit],
			Miss:          g.byDisp[DispMiss],
			Passthrough:   g.byDisp[DispPassthrough],
			Write:         g.byDisp[DispWrite],
			Error:         g.byDisp[DispError],
			ByReason:      byReason,
			PassQuery:     g.passQuery,
			Debounced:     g.debounced,
			UpstreamSaved: g.upstreamSaved,
			Sample:        g.sample,
			LastSeen:      g.lastSeen.Format(time.RFC3339),
		})
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Total != gs[j].Total {
			return gs[i].Total > gs[j].Total
		}
		return gs[i].Key < gs[j].Key
	})
	if len(gs) > max {
		gs = gs[:max]
	}
	return gs
}

// normalizeRoute maps a path onto its route shape (owner/repo/ref/sha/number
// segments become placeholders) so it groups with siblings. Total: any input
// yields a bounded route string, never an error.
func normalizeRoute(path string) string {
	segs := splitPathSegs(path)
	if len(segs) == 0 {
		return "/"
	}
	var out []string
	switch segs[0] {
	case "repos":
		out = normalizeRepoRoute(segs)
	case "orgs":
		out = normalizeOwnerRoute(segs, "{org}")
	case "users":
		out = normalizeOwnerRoute(segs, "{username}")
	case "app":
		// /app/installations/{id}[/access_tokens] (the token-mint route).
		if len(segs) >= 3 && segs[1] == "installations" {
			out = append([]string{"app", "installations", "{id}"}, normalizeTail(segs[3:], 1)...)
		} else {
			out = normalizeTail(segs, 2)
		}
	default:
		// Unknown top-level path (/graphql, /user, /rate_limit, /search/issues,
		// ...): group by the first two (normalized) segments + "…".
		out = normalizeTail(segs, 2)
	}
	return clampRoute("/" + strings.Join(out, "/"))
}

// normalizeRepoRoute handles /repos/{owner}/{repo} and its subresources.
func normalizeRepoRoute(segs []string) []string {
	switch len(segs) {
	case 1:
		return []string{"repos"}
	case 2:
		return []string{"repos", "{owner}"}
	case 3:
		return []string{"repos", "{owner}", "{repo}"}
	}
	return append([]string{"repos", "{owner}", "{repo}"}, normalizeRepoTail(segs[3:])...)
}

// normalizeRepoTail matches the known subresource grammars under a repo; tail
// is non-empty. Anything unrecognized falls through to the generic normalizer.
func normalizeRepoTail(tail []string) []string {
	switch tail[0] {
	case "compare": // basehead may carry slashes (branch names): greedy
		if len(tail) > 1 {
			return []string{"compare", "{basehead}"}
		}
	case "contents": // file path: greedy
		if len(tail) > 1 {
			return []string{"contents", "{path}"}
		}
	case "commits":
		return normalizeCommitsTail(tail)
	case "branches": // branch names may carry slashes: greedy
		if len(tail) > 1 {
			return []string{"branches", "{branch}"}
		}
	case "labels": // label names may decode to anything: greedy
		if len(tail) > 1 {
			return []string{"labels", "{name}"}
		}
	case "statuses":
		if len(tail) > 1 {
			return []string{"statuses", "{sha}"}
		}
	case "git":
		if len(tail) >= 3 && tail[1] == "commits" {
			return []string{"git", "commits", "{sha}"}
		}
	}
	// Everything else falls through to the generic {number}/{sha} + depth cap.
	return normalizeTail(tail, 3)
}

// commitRefSubresources match on the LAST segment, so a ref can carry slashes.
var commitRefSubresources = set.Of(
	"status", "check-runs", "statuses", "pulls", "check-suites", "comments",
)

func normalizeCommitsTail(tail []string) []string {
	if len(tail) == 1 {
		return tail // the commits LIST
	}
	if last := tail[len(tail)-1]; len(tail) >= 3 && commitRefSubresources.Contains(last) {
		return []string{"commits", "{ref}", last}
	}
	// Single-commit read: a full sha groups as {sha}, anything else (branch or
	// tag names, possibly slashed) as {ref}.
	if len(tail) == 2 && isHexSHA(tail[1]) {
		return []string{"commits", "{sha}"}
	}
	return []string{"commits", "{ref}"}
}

// normalizeOwnerRoute handles /orgs/{org}/... and /users/{username}/....
func normalizeOwnerRoute(segs []string, placeholder string) []string {
	if len(segs) == 1 {
		return segs[:1]
	}
	return append([]string{segs[0], placeholder}, normalizeTail(segs[2:], 2)...)
}

// normalizeTail turns numbers into {number}, 40-hex into {sha}, and anything
// past max segments into a trailing "…".
func normalizeTail(segs []string, max int) []string {
	n := len(segs)
	if n > max {
		n = max
	}
	out := make([]string, 0, n+1)
	for _, s := range segs[:n] {
		out = append(out, normalizeSeg(s))
	}
	if len(segs) > max {
		out = append(out, "…")
	}
	return out
}

func normalizeSeg(s string) string {
	if isHexSHA(s) {
		return "{sha}"
	}
	if isAllDigits(s) {
		return "{number}"
	}
	return s
}

// splitPathSegs splits a path on "/", dropping empty segments (leading,
// trailing, or doubled slashes) so any input yields clean segments.
func splitPathSegs(path string) []string {
	parts := strings.Split(path, "/")
	segs := parts[:0]
	for _, p := range parts {
		if p != "" {
			segs = append(segs, p)
		}
	}
	return segs
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// clampRoute bounds a route string (rune-safe) so one giant segment can't
// bloat a group key.
func clampRoute(route string) string {
	if len(route) <= routeMaxLen {
		return route
	}
	cut := routeMaxLen
	for cut > 0 && !utf8.RuneStart(route[cut]) {
		cut--
	}
	return route[:cut] + "…"
}
