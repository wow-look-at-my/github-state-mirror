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

// The implementation BRIEF: `GET /api/brief`, behind the dashboard's "Copy
// implementation brief" button.
//
// Modeling a new cached route used to start with a human copy-pasting the
// Requests tab into a chat window — which loses the two things that actually
// decide the design (the query shapes callers send, and the SHAPE of what
// GitHub answers) and carries a lot of noise that does not. This assembles the
// whole picture server-side, from the request-group counters (requestgroups.go)
// joined to the sampled shapes (shapes.go), and renders it as one Markdown
// document: the uncached routes ranked by traffic, each with its reasons,
// query-parameter names, callers, upstream statuses, and the key/type skeleton
// of the response — followed by this repo's own tier-2 checklist, so the brief
// is directly actionable rather than a pile of numbers.
//
// It contains no values: query parameters appear by NAME, response bodies as
// key/type skeletons. Admin-only, like every other operator view.

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

// briefCandidate is one uncached route: its counters joined to its shape.
type briefCandidate struct {
	requestGroupSnapshot
	Shape *routeShapeSnapshot `json:"shape,omitempty"`
}

// buildBrief joins the route groups to the captured shapes, keeping only
// routes that actually forward reads uncached (passthrough > 0) — a fully
// cached route has nothing to implement. Ranked by passthrough volume: the
// brief's whole job is to name the next thing worth modeling.
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

// briefView is the template's data. It is a flat, already-computed view: the
// template renders, it does not decide (the one exception is the trivial
// per-candidate arithmetic the FuncMap covers).
type briefView struct {
	GeneratedAt string
	Total       int64
	// Dispositions is the totals line, in a fixed reading order rather than
	// map order.
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

// briefTemplate is the document (brief.md.tmpl) -- kept as Markdown in its own
// file so editing a heading or a bullet is editing Markdown. text/template,
// NOT html/template: this is Markdown for a human and a model to read, and
// HTML-escaping would mangle every `<`, `&`, and quote in a captured path or
// skeleton.
//
//go:embed brief.md.tmpl
var briefTemplate string

// briefTmpl is parsed once at init: a template typo is then a startup panic in
// every test and every boot, not a broken response some Tuesday.
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
		// The template is parsed at init and its data is plain structs, so an
		// execution error means a template edit no test exercised. Say so in
		// the document rather than serving a silent half-brief.
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

// debounceVerdict names the one debounce reading that is otherwise easy to
// misread: held-with-nothing-saved is pure added latency, because the polls
// arrive further apart than the window.
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

// briefChecklist is this repo's own rules for adding a cached route, carried
// with the data so the brief is actionable on its own. It mirrors CLAUDE.md's
// cache-contract section; when that doctrine changes, change this too.
const briefChecklist = "## How to model one of these (the tier-2 contract)\n\n" +
	"Every data route is in exactly one tier. Tier 1 is the `/graphql` org-repos query " +
	"(byte-identical to GitHub, identity-test-locked — do not touch). Tier 3 is the verbatim " +
	"passthrough proxy, which is where everything above currently lives. Adding a cached route " +
	"means tier 2, whose contract is:\n\n" +
	"1. **Reveal gate first.** `h.reveal(r, owner, repo, denyKind…, resourceKey)` before any read: " +
	"public repo / live grant / cached deny verdict / live probe. A denied caller gets GitHub's own " +
	"relayed answer; an unrevealable transient error 502s that one request.\n" +
	"2. **Absorb state, rebuild trimmed — never byte-replay.** Parse the response into structured " +
	"columns and render the answer yourself. Drop every URL field (`url`, `*_url`, `_links`); the " +
	"recursive `assertNoURLKeys` test enforces it, and each exception needs a fresh consumer survey " +
	"(check the Callers line above for who to survey).\n" +
	"3. **Shape guard.** Model an explicit set of query parameters and only the default JSON " +
	"`Accept`; everything else calls `h.passthrough(w, r, PassQuery|PassAccept|PassPath)` so the " +
	"decline is counted with a reason instead of vanishing.\n" +
	"4. **Same bytes on hit and miss.** Render once at absorb time and store the rendered document, " +
	"or rebuild identically from rows; mark responses `X-GSM-Cache: hit|miss`.\n" +
	"5. **Explicit invalidation + TTL + pruning.** Name the webhook event that moves this resource " +
	"and flush on it in `internal/sync/webhook.go` (per-resource where the payload names it, " +
	"repo-wide otherwise), plus a TTL backstop for missed deliveries and LRU pruning against " +
	"`ghdata.CacheMaxRows`. A route with no invalidation signal is not a candidate — say so instead " +
	"of shipping it.\n" +
	"6. **Only cacheable answers absorb.** A 200 whose body the model can hold, plus any " +
	"authoritative verdict worth caching (a 404 \"absent\" answer often is). Transient failures " +
	"(5xx, 429) relay unstored, every time.\n\n" +
	"### Where the response shape comes from\n\n" +
	"The skeletons above are what THIS fleet actually received — the authority on which " +
	"answers really occur, which query shapes callers send, and who breaks if the rebuild trims a " +
	"field. They are not the only source: GitHub publishes an OpenAPI description of every " +
	"documented endpoint (`github/rest-api-description`, `descriptions/api.github.com/api.github.com.json`), " +
	"which gives the full schema including fields this traffic happened not to exercise, and their " +
	"nullability. For a documented endpoint, read the spec AND the capture; a route with no captured " +
	"skeleton is not necessarily an undocumented one.\n\n" +
	"### Files a new route touches\n\n" +
	"- `internal/database/schema.sql` — the snapshot table (+ unique key and LRU index), then bump " +
	"`SchemaVersion` in `internal/database/db.go` (there are no migrations: the cache is nuked and " +
	"recreated).\n" +
	"- `internal/database/queries/respcache.sql` — get/upsert/touch/invalidate/expire/prune, then " +
	"`sqlc generate`.\n" +
	"- `internal/ghdata/respcache_<route>.go` — the store methods.\n" +
	"- `internal/api/respcache_<route>.go` — the handler (guard → reveal → hit → fetch → absorb → " +
	"rebuild) and `internal/api/router.go` — the registration.\n" +
	"- `internal/sync/webhook.go` — the invalidation, wired into `invalidateResponseCaches` / " +
	"`invalidateForPush`.\n" +
	"- Tests: a hit/miss pair, the shape-guard passthroughs, the invalidation, and the no-URL-keys " +
	"assertion. Then `npm run build && go-toolchain`.\n\n" +
	"### When the answer is \"don't cache it\"\n\n" +
	"A route whose response describes a set that changes continuously and has no webhook naming the " +
	"change (a queued-work backlog, a live runner roster) is not a caching gap — it is the model " +
	"working. Record that verdict in CLAUDE.md rather than shipping a TTL that serves wrong answers.\n"

// handleBrief renders the implementation brief (brief.go): every uncached
// route joined to its captured request/response shape, plus the tier-2
// checklist — the whole input for modeling the next cached route, in one
// copyable document. Admin-only like every other operator view; the payload
// carries no values (query parameters by name, bodies as key/type skeletons).
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
