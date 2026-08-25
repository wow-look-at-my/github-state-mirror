# The implementation brief: capturing what a new cached route needs

`GET /api/brief` (admin-only), behind the "Capture implementation brief"
button under the Requests tab's "Top uncached requests" table.

## Why it exists

The route-shape counters (`internal/api/requestgroups.go`) answer *which*
uncached route is hottest and *why* it was forwarded. Modeling that route needs
two things they deliberately do not hold:

- the query shapes callers actually send, and who sends them;
- the SHAPE of what GitHub answers — without it, designing the trimmed rebuild
  means guessing GitHub's schema from memory or reading the calling service's
  source.

Before this, recovering those meant pasting the whole dashboard page into a
chat window, which loses exactly the parts that decide the design and carries a
lot that does not.

## What is captured

`internal/api/shapes.go` keeps a bounded in-memory store (300 route shapes, the
`requestLog`/`ratemeter` stance: a live operator view, not an audit log; resets
on restart). Per `(method, normalized route)`:

- the UNION of query-parameter **names**, each with a count;
- `Accept` headers seen;
- callers, by verified display name (or short principal key);
- upstream status counts;
- up to 3 concrete sample paths;
- per status, the response's **key/type skeleton**, its content type, and its
  real size.

**No value is ever retained.** `jsonSkeleton` reduces a body before anything is
stored: every scalar becomes its type name (`string`, `number`, `bool`,
`null` — nulls survive as `null` because a nullable field's key must still be
emitted by a rebuild), every array collapses to its first element's shape plus
the observed length, and objects keep only key names. A non-JSON body yields no
skeleton at all — which is itself the answer, since an opaque body has no state
to absorb and therefore cannot become a tier-2 route. Query parameters keep
their names and never their values, matching the existing `pass_query` rule (a
value can carry a credential; a name cannot).

Bounds: a body is sampled at most once per route shape per 30 minutes and only
up to 256 KiB (past that the capture is abandoned, not truncated — a truncated
body parses to nothing). Depth, key count, key length, and total skeleton size
are all clamped, so a pathological document cannot grow a group key.

## Where sampling hooks in

Two paths, which together cover every forwarded read:

- `recordPassthrough` (`requestlog.go`) — every request the proxy serves,
  whether it arrived via chi's NotFound or a cached route's `h.passthrough`
  bail-out. The `statusRecorder` buffers a bounded prefix only when the store
  asks for one; every byte still reaches the client immediately.
- `replayUnstored` (`respcache.go`) — the request was modeled but the RESPONSE
  was not. This never reaches the proxy, and it is exactly the class whose
  answer the brief most needs; the body is already buffered there.

## What the brief renders

`internal/api/brief.go` joins the route groups to the captured shapes, keeps
only routes that actually forward reads uncached (a fully cached route has
nothing to implement; a write-only group is not a caching gap), ranks them by
passthrough volume, and renders Markdown: per candidate the counts, the
passthrough reasons, the debounce reading (including the "held with nothing
saved" verdict — all cost, no benefit), the query names, callers, statuses,
sample paths, and the response skeletons — followed by this repo's tier-2
checklist and the list of files a new route touches.

The checklist is carried WITH the data on purpose: the output has to be
actionable on its own rather than a pile of numbers. It mirrors CLAUDE.md's
cache-contract section, and it offers no "don't cache it" outcome: a route that
still forwards is unfinished work, and a resource no delivery names today is a
missing invalidation signal to go add, not a verdict to record.

The JSON payload carries the same structure alongside the Markdown, so the
endpoint is usable programmatically.

The document itself is a `text/template` in `internal/api/brief.md.tmpl`,
embedded and parsed once at init (`template.Must`, so a syntax error is a
startup panic rather than a broken response some Tuesday). It lives in its own
file so editing a heading or a bullet is editing Markdown; everything computed
— percentages, counted-name lists, the debounce verdict — comes from the
FuncMap in `brief.go`, and the template renders rather than decides. It is
`text/template`, never `html/template`: the output is Markdown for a human and
a model to read, and HTML-escaping would mangle every `<`, `&`, and quote in a
captured path or skeleton.

## Compressed samples

Two paths carry the sample bytes: `recordPassthrough`'s `statusRecorder`
buffers what the reverse proxy writes to the client, and `replayUnstored`
buffers what it already read from GitHub. Neither the proxy nor the mirror
strips a caller's `Accept-Encoding` before forwarding, so GitHub answers
gzip-encoded whenever the caller asked for it — most HTTP clients do, by
default. A sample is therefore the WIRE body, not the decoded one.

`decodeSample` (`shapes.go`) gunzips a sample when the response's own
`Content-Encoding` says gzip, before `jsonSkeleton` ever sees it. Without this
step `json.Unmarshal` fails on every gzip sample, no skeleton is ever stored.

## Confirmed non-JSON is not the same as "not captured yet"

A second route to the same stall: some routes never answer with JSON at
all — a `.diff`/`.patch` representation, or a plain-text `401` from
`requireAuth` on an unauthenticated scan. `jsonSkeleton` correctly returns ""
for these (there is no key/type shape to model), but `observe` used to advance
`lastSampleAt` only inside the "skeleton succeeded" branch. A permanently
non-JSON status therefore never advanced the resample clock either, and
`wantsBody` asked for a fresh sample on every single subsequent passthrough
forever — the same "no response outline yet" stall as the gzip case, but for a
status that will never produce a skeleton no matter how long the operator
waits.

`observe` now advances `lastSampleAt` whenever a body was actually sampled,
whether or not it parsed as JSON, and records a confirmed-non-JSON status
separately (`routeShape.nonJSON`, surfaced as `shape.non_json` in the JSON
payload). The brief renders it as a permanent fact — "HTTP `n` is confirmed
not JSON... not a capture gap" — instead of implying a body will eventually
show up, and the dashboard's "N routes have no response outline yet" count
excludes any status already confirmed non-JSON.

## What the capture is not

It records what this fleet actually sends and receives. That makes it the
authority on which answers really occur, which query shapes callers use, and
who to survey before pinning a URL field — but it is not the only source of a
response schema. GitHub publishes an OpenAPI description of every documented
endpoint (`github/rest-api-description`), which carries the full schema
including fields this traffic happens not to exercise, and their nullability.
The Code Quality setup route was modeled from that spec before any sample had
been captured. **A route with no captured skeleton is not necessarily an
undocumented one** — check the spec first, then use the capture for the traffic
and for anything the docs do not cover. The brief's checklist says so too.
