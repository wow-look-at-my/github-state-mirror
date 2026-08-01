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
cache-contract section, including the case that matters most — when the honest
answer is "don't cache it", because the response describes a set that changes
continuously and no webhook names the change.

The JSON payload carries the same structure alongside the Markdown, so the
endpoint is usable programmatically.
