package api

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Request GROUPING for the dashboard's "Requests" tab: every recorded request
// also bumps a cumulative per-(method, route-shape) counter, so the operator
// can see the hottest routes — and the hottest UNCACHED routes (caching
// candidates) — instead of eyeballing the flat recent-requests ring. Groups
// live on the requestLog (same mutex, same in-memory/live-view stance, same
// since-restart semantics as the per-disposition totals — deliberately NOT
// windowed by the bounded recent ring, and NOT persisted).

const (
	// requestGroupsCap bounds the group map. The normalizer collapses real
	// traffic into a small fixed family of route shapes, so approaching the cap
	// means garbage paths; when full, NEW shapes are dropped (simple + a hard
	// memory bound) while every existing group keeps counting.
	requestGroupsCap = 1000
	// requestGroupsSnapshotCap caps the groups in one /api/requests payload.
	requestGroupsSnapshotCap = 100
	// routeMaxLen defensively clamps a pathological route string (a huge
	// segment in an unknown 1-2 segment path) so group keys stay small.
	routeMaxLen = 200
)

// routeGroup is the cumulative tally for one (method, route shape).
type routeGroup struct {
	method   string
	route    string
	total    int64
	byDisp   map[string]int64
	sample   string // one recent raw path, for identifying the shape
	lastSeen time.Time
}

// requestGroupSnapshot is one group in the /api/requests payload.
type requestGroupSnapshot struct {
	Key         string `json:"key"` // method + " " + route
	Method      string `json:"method"`
	Route       string `json:"route"`
	Total       int64  `json:"total"`
	Hit         int64  `json:"hit"`
	Miss        int64  `json:"miss"`
	Passthrough int64  `json:"passthrough"`
	Write       int64  `json:"write"`
	Error       int64  `json:"error"`
	Sample      string `json:"sample"`
	LastSeen    string `json:"last_seen"` // RFC3339
}

// bumpGroupLocked records one request into its group. Caller holds l.mu.
func (l *requestLog) bumpGroupLocked(method, route, rawPath, disposition string, now time.Time) {
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
	g.sample = rawPath
	g.lastSeen = now
}

// groupSnapshotsLocked returns the groups sorted by total desc (key asc on
// ties, for determinism), capped at max. Caller holds l.mu.
func (l *requestLog) groupSnapshotsLocked(max int) []requestGroupSnapshot {
	gs := make([]requestGroupSnapshot, 0, len(l.groups))
	for key, g := range l.groups {
		gs = append(gs, requestGroupSnapshot{
			Key:         key,
			Method:      g.method,
			Route:       g.route,
			Total:       g.total,
			Hit:         g.byDisp[DispHit],
			Miss:        g.byDisp[DispMiss],
			Passthrough: g.byDisp[DispPassthrough],
			Write:       g.byDisp[DispWrite],
			Error:       g.byDisp[DispError],
			Sample:      g.sample,
			LastSeen:    g.lastSeen.Format(time.RFC3339),
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

// normalizeRoute maps a concrete request path onto its route SHAPE, so
// requests differing only in owner/repo/ref/sha/number group together
// (e.g. /repos/a/b/compare/x...y -> /repos/{owner}/{repo}/compare/{basehead}).
// It is a dumb, total function: any input — empty, unrooted, doubled slashes,
// unicode, absurd depth — yields some bounded route string, never an error.
// Known GitHub grammars get contextual placeholders; inside any tail, numeric
// segments become {number} and 40-hex segments {sha}; unknown deep tails
// collapse to a trailing "…" so junk paths can't explode the group map.
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
	// pulls[/{number}[/files|merge|...]], issues/{number}, actions/runs,
	// installation, merges, ... — the generic rules ({number}/{sha} + depth
	// cap) produce the right shapes without per-route grammar.
	return normalizeTail(tail, 3)
}

// commitRefSubresources are the trailing-literal forms served under
// /repos/{owner}/{repo}/commits/{ref}/<sub>. The suffix anchor is what lets a
// ref carry slashes (mirroring the server's own subtree dispatcher), so the
// match keys on the LAST segment.
var commitRefSubresources = map[string]bool{
	"status": true, "check-runs": true, "statuses": true, "pulls": true, "check-suites": true, "comments": true,
}

func normalizeCommitsTail(tail []string) []string {
	if len(tail) == 1 {
		return tail // the commits LIST
	}
	if last := tail[len(tail)-1]; len(tail) >= 3 && commitRefSubresources[last] {
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

// normalizeTail generically normalizes a path tail: numeric segments become
// {number}, 40-hex segments {sha}, and anything deeper than max segments
// collapses to a trailing "…" — so unknown shapes group coarsely instead of
// minting one group per concrete path.
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
