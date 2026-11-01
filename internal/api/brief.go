// briefPayload is the JSON form.

package api

import (
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// GET /api/brief joins uncached-route counters to captured shapes.
// see docs/dashboard/implementation-brief.md

// briefPayload is the JSON form. Markdown is the deliverable the button
// copies; the structured fields ride along so the same endpoint is usable
// programmatically.
type briefPayload struct {
	GeneratedAt string           `json:"generated_at"`
	Total       int64            `json:"total"`
	Totals      map[string]int64 `json:"totals"`
	Candidates  []briefCandidate `json:"candidates"`
	Markdown    string           `json:"markdown"`
}

// briefCandidate is uncached route: its counters joined to its shape.
type briefCandidate struct {
	requestGroupSnapshot
	Shape *routeShapeSnapshot `json:"shape,omitempty"`
}

// buildBrief joins the route groups to the captured shapes, keeping only
// routes that forward reads uncached (passthrough >) — a fully cached route
// has nothing to implement. Ranked by passthrough volume: the brief's whole
// job is to name the next thing worth modeling.
func buildBrief(snap requestLogSnapshot, shapes map[string]routeShapeSnapshot, limit int) []briefCandidate {
	out := make([]briefCandidate, 0, len(snap.Groups))
	for _, g := range snap.Groups {
		if g.Passthrough == 0 {
			continue
		}
		c := briefCandidate{requestGroupSnapshot: g}
		if sh, ok := shapes[g.Key]; ok {
			shCopy := sh
			c.Shape = &shCopy
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Passthrough != out[j].Passthrough {
			return out[i].Passthrough > out[j].Passthrough
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// briefView is the template's data.
type briefView struct {
	GeneratedAt string
	Total       int64
	// Fixed reading order, not map order.
	Dispositions []dispositionCount
	// PassthroughTotal is the denominator for "% of all passthroughs".
	PassthroughTotal int64
	Candidates       []briefCandidate
	Checklist        string
}

type dispositionCount struct {
	Name  string
	Count int64
}

//go:embed brief.md.tmpl
var briefTemplate string

// Parsed at init, so a template syntax error panics at startup, not at request time.
var briefTmpl = template.Must(template.New("brief").Funcs(briefFuncs).Parse(briefTemplate))

// briefFuncs are the computations the document needs. Each is a pure function
// over already-collected data; nothing here reaches for state.
var briefFuncs = template.FuncMap{
	"pct":     pct,
	"ordinal": func(i int) int { return i + 1 },
	"add": func(ns ...int64) int64 {
		var sum int64
		for _, n := range ns {
			sum += n
		}
		return sum
	},
	"rest": func(ss []string) []string {
		if len(ss) < 2 {
			return nil
		}
		return ss[1:]
	},
	"backtickList": func(ss []string) string {
		return "`" + strings.Join(ss, "`, `") + "`"
	},
	"names":           namesLine,
	"counts":          joinCounts,
	"statuses":        statusesLine,
	"debounceVerdict": debounceVerdict,
	"orDash":          orDash,
}

// renderBrief renders the Markdown document. Paste-ready by construction: no
// ANSI, every section self-describing, the checklist last.
func renderBrief(snap requestLogSnapshot, cands []briefCandidate, generatedAt string) string {
	view := briefView{
		GeneratedAt:      generatedAt,
		Total:            snap.Total,
		PassthroughTotal: snap.ByDisposition[DispPassthrough],
		Candidates:       cands,
		Checklist:        briefChecklist,
	}
	for _, d := range []string{DispHit, DispMiss, DispPassthrough, DispWrite, DispError} {
		if n, ok := snap.ByDisposition[d]; ok {
			view.Dispositions = append(view.Dispositions, dispositionCount{Name: d, Count: n})
		}
	}
	var b strings.Builder
	if err := briefTmpl.Execute(&b, view); err != nil {
		// An execution error here means an untested template edit; say so in the document.
		slog.Error("render implementation brief failed", "error", err)
		return "# github-state-mirror — uncached traffic brief\n\nThe brief template failed to render: " + err.Error() + "\n"
	}
	return b.String()
}

func namesLine(cs []countedName) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("`%s` ×%d", c.Name, c.Count))
	}
	return strings.Join(parts, ", ")
}

func statusesLine(ss []countedInt) string {
	parts := make([]string, 0, len(ss))
	for _, s := range ss {
		parts = append(parts, fmt.Sprintf("%d ×%d", s.Value, s.Count))
	}
	return strings.Join(parts, ", ")
}

func joinCounts(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("`%s` ×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// debounceVerdict names the debounce reading that is otherwise easy to
// misread. It held-with-nothing-saved is pure added latency, because the
// polls arrive further apart than the window.
func debounceVerdict(held, saved int64) string {
	switch {
	case held == 0:
		return ""
	case saved == 0:
		return " — **all cost, no benefit**: polls arrive further apart than the debounce window"
	case saved*10 < held:
		return " — coalescing rarely fires"
	default:
		return ""
	}
}

func pct(n, total int64) string {
	if total <= 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(total))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

//go:embed brief-checklist.md
var briefChecklist string

// handleBrief renders the implementation brief (brief.go). Every uncached
// route joined to its captured request/response shape, plus the tier-
// checklist — the whole input for modeling the next cached route. That
// route is in copyable document. Admin-only like every other operator view;
// the payload carries no values (query parameters by name, bodies as key/type
// skeletons).
func (d *dashboard) handleBrief(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	limit := briefDefaultCandidates
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "invalid 'limit' query parameter", http.StatusBadRequest)
			return
		}
		limit = min(n, briefMaxCandidates)
	}
	snap := d.reqlog.snapshot(0)
	cands := buildBrief(snap, d.shapes.snapshot(), limit)
	generated := time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, briefPayload{
		GeneratedAt: generated,
		Total:       snap.Total,
		Totals:      snap.ByDisposition,
		Candidates:  cands,
		Markdown:    renderBrief(snap, cands, generated),
	})
}

const (
	briefDefaultCandidates = 20
	briefMaxCandidates     = 100
)
