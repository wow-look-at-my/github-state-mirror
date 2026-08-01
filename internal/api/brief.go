package api

import (
	"fmt"
	"sort"
	"strings"
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

// renderBrief writes the Markdown document. Keep it paste-ready: no ANSI, no
// tables wider than they need to be, every section self-describing.
func renderBrief(snap requestLogSnapshot, cands []briefCandidate, generatedAt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# github-state-mirror — uncached traffic brief\n\n")
	fmt.Fprintf(&b, "Generated %s. Counters are cumulative since the process last restarted.\n\n", generatedAt)

	fmt.Fprintf(&b, "## Traffic since restart\n\n")
	fmt.Fprintf(&b, "%d requests — ", snap.Total)
	parts := make([]string, 0, len(snap.ByDisposition))
	for _, d := range []string{DispHit, DispMiss, DispPassthrough, DispWrite, DispError} {
		if n, ok := snap.ByDisposition[d]; ok {
			parts = append(parts, fmt.Sprintf("%s %d (%s)", d, n, pct(n, snap.Total)))
		}
	}
	fmt.Fprintf(&b, "%s\n\n", strings.Join(parts, ", "))

	fmt.Fprintf(&b, "## Uncached routes, by passthrough volume\n\n")
	if len(cands) == 0 {
		b.WriteString("No route has forwarded a read uncached since restart.\n\n")
	}
	for i, c := range cands {
		fmt.Fprintf(&b, "### %d. `%s`\n\n", i+1, c.Key)
		fmt.Fprintf(&b, "- **Passthrough** %d of %d total (%s of this route; %s of all passthroughs)\n",
			c.Passthrough, c.Total, pct(c.Passthrough, c.Total), pct(c.Passthrough, snap.ByDisposition[DispPassthrough]))
		if len(c.ByReason) > 0 {
			fmt.Fprintf(&b, "- **Why uncached**: %s\n", joinCounts(c.ByReason))
		}
		if c.Hit+c.Miss+c.Error > 0 {
			fmt.Fprintf(&b, "- Other dispositions: hit %d, miss %d, error %d\n", c.Hit, c.Miss, c.Error)
		}
		if c.Debounced > 0 || c.UpstreamSaved > 0 {
			fmt.Fprintf(&b, "- Debounce: held %d, upstream calls saved %d%s\n", c.Debounced, c.UpstreamSaved,
				debounceVerdict(c.Debounced, c.UpstreamSaved))
		}
		if c.PassQuery != "" {
			fmt.Fprintf(&b, "- Sampled passthrough query (names only): `?%s`\n", c.PassQuery)
		}
		if c.Sample != "" {
			fmt.Fprintf(&b, "- Sample path: `%s`\n", c.Sample)
		}
		if c.Shape != nil {
			writeShape(&b, c.Shape)
		} else {
			b.WriteString("- _No response shape captured yet — it is sampled on the next passthrough for this route._\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(briefChecklist)
	return b.String()
}

func writeShape(b *strings.Builder, sh *routeShapeSnapshot) {
	if len(sh.QueryNames) > 0 {
		fmt.Fprintf(b, "- Query parameter names seen (union, never values): %s\n", namesLine(sh.QueryNames))
	}
	if len(sh.Accepts) > 0 {
		fmt.Fprintf(b, "- Accept headers seen: %s\n", namesLine(sh.Accepts))
	}
	if len(sh.Callers) > 0 {
		fmt.Fprintf(b, "- Callers: %s\n", namesLine(sh.Callers))
	}
	if len(sh.Statuses) > 0 {
		parts := make([]string, 0, len(sh.Statuses))
		for _, s := range sh.Statuses {
			parts = append(parts, fmt.Sprintf("%d ×%d", s.Value, s.Count))
		}
		fmt.Fprintf(b, "- Upstream statuses: %s\n", strings.Join(parts, ", "))
	}
	if len(sh.SamplePaths) > 1 {
		fmt.Fprintf(b, "- More sample paths: `%s`\n", strings.Join(sh.SamplePaths[1:], "`, `"))
	}
	for _, body := range sh.Bodies {
		if body.Skeleton == "" {
			continue
		}
		fmt.Fprintf(b, "\n**Response shape — HTTP %d** (%s, %d bytes, keys and types only):\n\n```\n%s\n```\n",
			body.Status, orDash(body.ContentType), body.Bytes, body.Skeleton)
	}
}

func namesLine(cs []countedName) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("`%s` ×%d", c.Name, c.Count))
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
