# /api/timeline: why it is 25 MB, and what the wire format should be

## The problem

`GET /api/timeline` with no `?since=` cursor serializes the WHOLE ring:
`reqtimeline` retains 24 h capped at `DefaultMaxEvents` = 100,000 events, and
one event costs ~283 B as JSON. That is ~27 MB uncompressed, sent to paint a
chart whose first viewport is one hour (`INITIAL_WINDOW_MS`, timeline.ts).

The origin is grey-clouded (Cloudflare proxying was turned off after
throttling), so nothing in front of the app compresses it. Compression is the
app's job now.

## What was measured

A bake-off, in a throwaway module (`prototype/timelinewire/`, own `go.mod` so
the repo build never saw its bson/protobuf deps). It generated a realistic
window (100k events over 24 h: 18 route shapes, 12 webhook event types, 220
repos, 34 principals, unique delivery GUIDs on webhook events, skewed
durations), round-tripped it through every candidate codec (a lossy format is
not a small format, it is a wrong one), and reported size + time — the tables
below.

**That module is gone, deliberately.** It was a prototype, it answered its
question, and the answer is these tables plus the format that shipped. Keeping
a second Go module with protobuf and bson dependencies to re-derive a decision
already made is dead weight. To re-run it, check out `4a78cb8^` — nothing since
then changed the shape of the data it measured.

What survives in this repo is `internal/api/testdata/browsercheck.ts`, the
real-browser harness — the only place the whole pipeline (fetch → library
decode → our intervals → component draw) is measured together. It is
TypeScript, type-checked against `web/src/` by `npm run check:harness`, so a
changed export fails the build rather than surfacing mid-measurement.

The node harnesses that drove the codec (`framecheck.ts`, `shipbench.ts`) are
gone with the codec itself — see the next section.

Codecs: **json** (today), **bson** (`mongo-driver/v2`), **protobuf**
(`timeline.proto`, hand-encoded with `protowire` — byte-identical to what
protoc-gen-go emits for that schema), **row** (hand-rolled: one record per
event, shared string dictionary, delta+varint ids/timestamps, presence mask),
**columnar** (hand-rolled: field-major, per-column dictionary, delta+varint
numeric columns, bitset for bools, empty columns omitted entirely).

Compressors are limited to what a browser decodes natively from
`Content-Encoding`, so the client pays nothing for them: gzip, zstd, br.

### Full ring (100,000 events) — what the endpoint dumps today

| codec | compress | raw | on-wire | B/event | enc ms | cmp ms |
|---|---|---:|---:|---:|---:|---:|
| json | none | 27.01 MB | 27.01 MB | 283.2 | 153 | – |
| json | gzip-1 | 27.01 MB | 3.82 MB | 40.1 | 153 | 90 |
| json | gzip-6 | 27.01 MB | 3.02 MB | 31.6 | 153 | 152 |
| json | zstd-fast | 27.01 MB | 3.33 MB | 34.9 | 153 | 123 |
| json | br-4 | 27.01 MB | 2.83 MB | 29.7 | 153 | 953 |
| bson | gzip-6 | 26.54 MB | 3.16 MB | 33.2 | 218 | 158 |
| protobuf | none | 14.55 MB | 14.55 MB | 152.6 | 54 | – |
| protobuf | gzip-6 | 14.55 MB | 2.43 MB | 25.5 | 54 | 109 |
| protobuf | br-9 | 14.55 MB | 1.78 MB | 18.7 | 54 | 2115 |
| row | none | 1.93 MB | 1.93 MB | 20.2 | 56 | – |
| row | gzip-6 | 1.93 MB | 1014 KB | 10.4 | 56 | 39 |
| **columnar** | **none** | **2.24 MB** | **2.24 MB** | **23.5** | **160** | – |
| **columnar** | **gzip-1** | 2.24 MB | 1.26 MB | 13.2 | 160 | **9** |
| **columnar** | **gzip-6** | 2.24 MB | 1.01 MB | 10.6 | 160 | 29 |
| **columnar** | **br-4** | 2.24 MB | **942 KB** | **9.6** | 160 | 89 |

### One hour (4,200 events) — what the chart actually paints on first load

| codec | compress | raw | on-wire | B/event |
|---|---|---:|---:|---:|
| json | none | 1.13 MB | 1.13 MB | 281.9 |
| json | gzip-6 | 1.13 MB | 129 KB | 31.4 |
| protobuf | gzip-6 | 622 KB | 105 KB | 25.6 |
| row | gzip-6 | 90 KB | 48 KB | 11.6 |
| columnar | gzip-6 | 104 KB | 52 KB | 12.7 |
| columnar | br-4 | 104 KB | 42 KB | 10.3 |

### Browser-side decode (node 22 = V8 = Chrome), 100k events

Decompression is excluded on purpose: the browser inflates `Content-Encoding`
in C++ off the main thread, so it is the same cost for every candidate. What
is measured is JS main-thread work to produce the chart's interval objects.

| path | best of 5 |
|---|---:|
| `JSON.parse` alone | 174.0 ms |
| `JSON.parse` + `map(toInterval)` (today) | 274.7 ms |
| columnar → intervals, events materialized eagerly | 66.9 ms |
| columnar → intervals, event objects built lazily on hover | **53.8 ms** |
| row → intervals | 117.3 ms |

The bake-off's `bench.mjs` asserted all three paths produced identical
intervals (0 mismatches) — a format that decodes to different data is not a
faster format.

## Findings

1. **JSON's cost is timestamps and repeated keys, not the data.** ~283 B/event
   carries maybe 25 B of information. `"start":"2026-08-01T12:07:29.123456Z"`
   alone is ~35 B for a number that delta-codes to 1–2 bytes.
2. **BSON is not a compression format.** 26.5 MB raw and *worse* than JSON
   under every compressor (its length-prefixed keys repeat per document, so
   there is nothing for the dictionary to win that gzip was not already
   winning). It also loses the one advantage JSON has — native browser
   parsing. Rejected outright.
3. **Protobuf halves the raw bytes** (14.55 MB) and encodes fastest of the
   schema formats, but it is still key-tagged per field per record; after gzip
   it lands within 20% of JSON. In the browser it also costs a decoder library
   (protobufjs) or generated code, plus a schema build step in a repo whose
   front-end deliberately has no bundler. Not worth it here.
4. **The hand-rolled formats win by an order of magnitude raw** (1.9–2.2 MB,
   ~13× smaller than JSON) because they attack the actual redundancy:
   a lane/route/actor string appears 100,000 times and should be one dictionary
   entry plus a one-byte index; ids and timestamps are monotonic and should be
   deltas.
5. **Columnar beats row where it matters.** Row is marginally smaller raw
   (1.93 vs 2.24 MB — its presence mask skips absent fields, where columnar
   writes an index for every row of every used column), but columnar
   compresses **3× faster** (9 ms vs 18 ms at gzip-1: like values sit adjacent,
   so the matcher works at short distances), lands the same or smaller
   on-wire, and decodes **2× faster in JS** (54 ms vs 117 ms) with no
   per-record branching. Columnar also permits the lazy-detail path: the chart
   needs 100k intervals now and one full event object on hover.
6. **Compression is not optional and gzip is nearly free at this shape.**
   columnar+gzip-1 costs 9 ms of server CPU for the full ring; on the realistic
   one-hour payload it is under 1 ms.

## Where the format lives

**The codec is in js-snippets** (`src/ui/timeline-wire.ts`), next to the
`<timeline-view>` component it exists to feed. That is where it belongs: a
chart holding 100k intervals needs a way to receive them that is not 27 MB of
JSON, and a decode that fits in a frame — both are properties of the timeline,
not of this particular consumer. The browser imports it at runtime from the
library site exactly like the component.

The split is LAYOUT vs VOCABULARY:

- **js-snippets owns the layout** — magic, varints, dictionaries, bitsets,
  columns — plus the one-chunk-per-frame driver. It takes the column names as
  a schema parameter, so it carries none of this mirror's words.
- **The mirror owns the vocabulary and the meaning** — which 13 columns it
  sends (`SCHEMA` in `web/src/timeline.ts`), and `intervalsGen`/`eventAt`,
  which turn columns into intervals with labels, states and lanes. None of
  that is wire format; all of it is what a github-mirror event *is*.

The wire bytes did not change when it moved: the column names were never on
the wire.

**The Go encoder stays here** (`internal/api/timelinewire.go`) — it is server
code, and js-snippets is a pnpm/ts0 library with no Go. Two implementations of
one layout in two repos are held together by BYTES:
`timelinewire_golden_test.go` asserts the encoder emits an exact payload for a
fixed snapshot, and js-snippets' `timeline-wire.test.ts` decodes that same
base64 and asserts the same values. Neither can drift without one going red,
and neither runs the other's toolchain — which the old arrangement did, with a
Go unit test executing a built browser bundle.

## Two encodings, one of which the chart refuses

`GET /api/timeline` serves the columnar payload to a caller that names its
media type exactly, and readable JSON to everyone else — curl, a browser, an
operator with `jq`. The endpoint stays inspectable by hand.

The first cut had the same two encodings and was still wrong, because the
CLIENT fell back: `fetchDecoded` decoded whatever came back. So an `Accept`
that drifted — a header edit, a proxy rewriting it, a `q=` mixup — silently
took the decode costing ~10x the frames, with nothing failing and nothing
logged. **The defect was the fallback, not the JSON.**

Deleting the JSON was the wrong correction (briefly made, then reverted): it
served a real consumer — a human debugging an admin endpoint — and dropping it
made `curl` return binary for no safety gain. The right correction is one line
in the client: send only the wire type, and THROW on any other content type,
which surfaces as the chart's existing "feed is down" state.

Both halves are pinned, and they are meant to be read together:

- `TestTimeline_ColumnarNegotiated` / `TestTimeline_ReadableByDefault` — the
  server's two answers.
- `TestTimelineClientRefusesJSON` — drives the REAL BUILT `assets/timeline.js`
  under node with a stubbed `fetch` returning exactly what the server sends a
  JSON caller, and requires `fetchDecoded` to reject it. Verified to fail when
  the fallback is put back.

`pageFromJSONGen` therefore has one consumer, the demo preview:
`assets/demo-data.js` is a hand-maintained JS literal that cannot be binary,
and it reaches the element through a fetcher override, never through this
endpoint. `shipbench.ts` also drives it, which keeps the measurement that
chose columnar reproducible.

## The frame budget (operator requirement)

> "make sure the average loading workload per frame for the duration of the
> load is <8ms per chunk, and load 1 chunk per frame"

The columnar decode was 63 ms in ONE synchronous task — four times better than
the JSON path it replaced, and still eight dropped frames. Meeting the budget
took three changes, in ascending order of importance:

1. **One chunk per frame, where a chunk is A FRAME'S WORTH OF WORK.** That
   second clause is the whole thing. `runSliced` claims a frame, then pulls
   generator steps — across phase boundaries — until `CHUNK_MS` (4) of real
   work is spent. One unit per frame means the unit's cost IS the frame's load,
   so a chunk under budget keeps every frame under budget.

   Defining a chunk as *one step of one phase* instead — each phase steering
   its own step size toward a time target, yielding a frame at each — is what
   the first cut did, and it took **70 frames (~1.2 s) to do 8 ms of work**:
   there are ~23 phases, each paid at least one frame, and the resulting chunks
   averaged 0.13 ms against an 8 ms budget. The step size was not the lever
   (target 3 ms and 6 ms both gave 70 frames); loosening the growth caps only
   reached 51, because the phase count was the floor. Filling the frame instead
   took it to **3 frames**.

   `STEP` is therefore a fixed granularity, not a size to tune: it bounds the
   OVERSHOOT, since the clock is read between steps. 1024 rows is ~1.7 ms of
   the dearest phase (the JSON transpose, ~1.7 us/row) and ~0.05 ms of the
   cheapest, so `CHUNK_MS` plus a worst step stays inside 8.
2. **A frame is a global TURN, and it is claimed BEFORE the work.** Neither
   half is optional. A live poll and a history load are separate tasks, so each
   taking "its own chunk per frame" put two chunks in one frame; and yielding
   *after* working leaves seams — a task's last chunk and the next flow's first
   one both run unwaited. Measured with chunks that actually fill a frame, that
   was 9.4 ms of our work in one frame (slices of 3.0 + 3.5 + 2.9). Waiters now
   queue for distinct frames, the wait confirms the frame counter moved
   (`rAF`+`setTimeout(0)` can otherwise resume inside the frame it yielded),
   and every unit claims its frame first.
3. **Not holding 100k intervals.** This is the one that actually mattered.
   With the full ring in memory the worst slice still spiked to 19 ms, and
   `--trace-gc` named the cause: 4-7 ms scavenges, triggered by 100k live
   interval objects, landing inside decode slices. No amount of yielding hides
   a GC pause. So the chart now FETCHES THE HOUR IT PAINTS
   (`?from=`/`?to=`, served by `reqtimeline.SnapshotRange`) and pulls older
   ranges through the component's `loadRange` hook as the viewport reaches
   them, each clamped to an hour. Dropping the per-row `{columns, row}`
   reference — the tooltip binary-searches the id column instead — removed
   another 100k allocations.

Measured on the real first-paint payload (4,166 events, one hour of realistic
traffic) through the SHIPPED module: **~3 ms of work per frame on average over
3 frames**, worst chunk ~4 ms. The JSON fallback over a full 100k-event window
averages 4.4 ms per frame over 34-39 frames. `TestTimelineFrameBudget` gates on the average — that is the
requirement — and bounds the worst single chunk at 2× the budget: a chunk is
sized from what the previous one cost, so a GC pause landing inside one is not
a sizing defect, but a chunk twice the budget is.

### What node could not see

`framecheck.ts` measures our decoder. The chart also hands intervals to the
`<timeline-view>` component, which ingests, packs sub-tracks and renders — on
the same main thread, and node has no component. So
`internal/api/testdata/browsercheck.ts` boots the real pieces in headless
Chromium (the built `assets/timeline.js`, the real `<gsm-timeline>` element,
the real component, a local server answering `/api/timeline` exactly as the
mirror does) and times every call the adapter makes into the component.

It immediately found a real defect the node harness could not: the apply loop
ran a decode slice AND several component merges **in one task** — 3 ms of
decode plus four merges is a 25 ms stall. A feed is now a chunk like any other
— one per frame — sized by what the last one actually cost (`TARGET_FEED_MS`,
capped at 512): a merge costs ~2.8 us per interval and gets dearer as the chart
fills, so an estimate taken from an early cheap call overshoots later ones — a
fixed 1024 measured 7.6 ms.

### Where the frames actually go (browser, real component)

`browsercheck.ts` boots the real pieces in headless Chromium — the built
`assets/timeline.js`, the real `<gsm-timeline>`, the real `<timeline-view>`,
a local server answering `/api/timeline` as the mirror does. It buckets every
recorded slice of OUR work into the frame it landed in, adds that frame's rAF
work, and reports the total: that sum is what "blocking a frame" means.

Across runs, **exactly one slice of ours lands per frame**, peaking at
4.0-5.4 ms; steady state 1.9-2.7 ms; interaction (24 zoom steps + 24 pans on
the loaded chart) 3.5-5.8 ms.

One frame per run does exceed 8 ms: **13-20 ms at t≈90-120 ms, before any data
exists**, with 0 ms of our loading work in it. Long-animation-frame attribution
names it rather than leaving it to guesswork — 18 ms under `resolve-promise` in
`timeline.js`, which is `boot()` resuming after the component's dynamic import
and calling `appendChild`, synchronously running the element's constructor and
`connectedCallback` (shadow DOM, canvas, adopted stylesheet). The browser
reports `blockingDuration: 0`. It is one-time element construction at boot, not
loading workload, and it is stated here rather than hidden behind a metric that
excludes it.

Getting there meant fixing three things:

1. **One chunk per frame, feeds included.** A feed makes the component rebuild
   and redraw, so it is a unit of work like a decode chunk and takes its own
   frame. The earlier design metered each frame's *remainder* against the
   component's measured work — a `FramePacer` with fed-frame debt and per-unit
   headroom estimates. All of that existed to answer "how much more may we
   spend in this frame", a question that only arises when several units share
   a frame. Putting one unit in a frame deletes the question and 145 lines.
2. **Resume in a fresh task after the frame.** `requestAnimationFrame` then
   `setTimeout(0)`: resuming inside the rAF callback would put the next chunk
   in the same frame as the component's redraw.
3. **Upstream, in js-snippets** (`claude/timeline-api-nulls-size-1zp5u5`):
   incremental per-lane rebuild instead of whole-chart work per merge; batched
   cluster-marker paths capped at a measured 32 per path (batching REVERSES
   past a few hundred: 1.75 us/marker at 8-32, 8.08 with 1200 in one path);
   batched lane separators; memoized label fitting; the cold-surface split
   (the first draw establishes the canvas and warms text shaping at EVERY size
   a lane label can use, the next one renders — warming only the base size left
   an 8 ms `drawLanes` on the first data-bearing frame, now 0.9 ms); and the
   rebuild/paint split. Total draw over a load: 57-59 ms ->
   39-45 ms, with rendering unchanged (pixel diff 0.089%, max channel delta 10).

### Two limits, stated rather than papered over

- **Frame gaps in this sandbox are not our signal.** A control run — same page,
  same component, ZERO events — shows 25-53 ms frame gaps from headless
  Chromium's own startup and container scheduling. The browser harness can
  measure our synchronous work reliably; it cannot resolve an 8 ms budget in
  wall-clock frame timing here.
- **The measurements are from a software rasterizer.** This container has no
  GPU, so canvas work here is several times dearer than on a real machine —
  the numbers above are pessimistic, not flattering. A control run with ZERO
  events is what proved the old first-paint spike was the surface and not the
  data.

## What shipped

Both halves of the recommendation below, minus the windowing (the operator's
call: "it doesn't matter if you send the events in 1hr chunks or 24hr all at
once"). The endpoint still answers the full cursor-selected window — it is
just an order of magnitude smaller and ~5x cheaper to render.

- **`internal/api/timelinewire.go`** — the columnar encoder, magic `TLC1`,
  media type `application/vnd.gsm.timeline.v1`. Selected by an EXACT Accept
  match, so curl, the demo preview and any browser's `*/*` keep getting JSON.
  The layout is versioned in both the magic and the media type and is never
  evolved in place: a change is v2.
- **`internal/api/compress.go`** — gzip at BestSpeed for the dashboard's
  buffered admin payloads, since the grey-clouded origin has nothing in front
  of it. Level 1 rather than 6 because on this shape it is 1006 KB in 24 ms
  against 952 KB in 140 ms (measured in the retired bake-off module).
  Deliberately NOT wired into the GitHub data plane: the cached routes have a
  pinned response-header contract, the proxy relays GitHub's own encoding, and
  the consistency check's NDJSON must keep flushing per line.
- **`internal/api/web/src/timeline.ts`** — the browser decoder. The JSON
  fallback decodes into the SAME column representation, so intervals, lanes
  and tooltips have exactly one shape to handle. Intervals are built straight
  from columns; the 19-field event object is materialized only when a tooltip
  asks for a row (`eventAt`).
- **`reqtimeline.SnapshotRange` + `?from=&to=`** — the windowed read behind the
  chart's async history. `since` and `from`/`to` are mutually exclusive (they
  answer different questions; silently ANDing them would let a history request
  come back empty because the live cursor had moved past it), and a windowed
  read still reports the live `max_id` — history never advances the cursor, or
  events recorded between the last poll and the history fetch would be skipped.
- **Browser harness.** `internal/api/testdata/browsercheck.ts` (needs
  `npm i playwright`; run it against a dumped payload) — the only check that
  sees the component. Deliberately NOT in CI: it needs a browser download and
  its frame-gap numbers are environment-dominated. The CI gate is the node
  one, which is deterministic.
- **Tests.** `timelinewire_test.go` round-trips the encoder against a Go
  decoder that mirrors the TS one (and asserts the size claim, so a regression
  to per-event strings fails rather than merely disappoints).
  `timelinewire_cross_test.go` runs the REAL BUILT `assets/timeline.js` under
  node against a payload this package encoded and compares every field of
  every event — the only check that the two languages actually agree.
  `timeline_test.go` covers negotiation, the JSON default, gzip (including a
  `gzip;q=0` refusal) and the small-payload floor.

### Measured on what shipped

A realistic full ring — 100,000 events over 24 h, 84% requests / 11% webhook
deliveries carrying UNIQUE delivery GUIDs / 5% notifications — written by
`TestTimelineWireDumpPayloads` and decoded by the REAL BUILT
`assets/timeline.js` (`internal/api/testdata/shipbench.ts`, node 22 = V8 =
Chrome, best of 5):

| | on the wire | per event | browser decode |
|---|---:|---:|---:|
| JSON (before) | 27.70 MB → **2.80 MB** gzipped | 277 B | **315.1 ms** |
| columnar (now) | 2.38 MB → **749 KB** gzipped | 23.8 B raw / 7.5 B gzipped | **63.2 ms** |

**3.7× fewer bytes on the wire, 5× less main-thread time**, carrying exactly
the same events (`shipbench.ts` asserts the wire and JSON paths produce
identical intervals; the Go/node cross-decode test asserts every field).

The decoder's first cut measured 85 ms; the bulk varint readers
(`WireReader.uvarints`/`varintSums`/`uvarintSums`) took it to 63 by inlining
the single-byte case rather than paying a call per value — nearly every value
on the wire is one byte, and there are ~1.8M of them in a full window.

Also measured end to end in `TestTimeline_GzipWhenAccepted` on 2,000 uniform
events: JSON 495,009 B → columnar+gzip 271 B.

## What the bake-off recommended, and what shipped

The recommendation was two independent changes, in this order of value — both
are now in place, so this section is the record of the reasoning, not a plan:

1. **Bound the uncursored response** so first paint fetches the hour it draws,
   and let the component's lazy-history path pull older windows on demand.
   27 MB → 1.1 MB on its own. *Shipped* as `?from=`/`?to=` over
   `reqtimeline.SnapshotRange`, plus the `loadRange` history hook.
2. **Compression in the app**, since the origin is grey-clouded. 1.1 MB →
   129 KB. *Shipped* as `internal/api/compress.go` — gzip only, at BestSpeed,
   rather than the negotiated zstd→br→gzip ladder first sketched: every browser
   takes gzip, so there is no negotiation to get wrong, and on this shape
   level 1 costs a sixth of level 6's CPU for 6% more bytes.

With both, JSON alone would have been tolerable (129 KB, 20 ms of JS). The
columnar format was still worth its ~120 lines of encoder and ~90 of TS
decoder: **columnar + gzip** is 1.01 MB and 54 ms of main-thread work against
JSON+gzip's 3.02 MB and 275 ms — 3× the bytes and 5× the CPU — and it is what
makes holding a wide window in view survivable at all.

The one recommendation NOT followed as written is "keep the JSON path as the
fallback": the JSON stays, but as an answer to callers that ask for it, never
as something the chart falls back to. See "Two encodings, one of which the
chart refuses" above for why that distinction is the whole safety argument.
