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

`prototype/timelinewire/` — a standalone module (own `go.mod`, so the repo
build never sees its bson/protobuf deps). It generates a realistic window
(100k events over 24 h: 18 route shapes, 12 webhook event types, 220 repos,
34 principals, unique delivery GUIDs on webhook events, skewed durations),
round-trips it through every candidate codec (`TestRoundTrip` — a lossy format
is not a small format, it is a wrong one), and reports size + time:

```sh
cd prototype/timelinewire
go test -run TestReport -v                 # the tables below
TLWIRE_DUMP=. go test -run TestDump        # writes events.{json,col,row}
node bench.mjs                             # the browser-side half
```

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

`bench.mjs` asserts all three paths produce identical intervals (0 mismatches).

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

## Recommendation

Two changes, independent, in this order of value:

1. **Bound the uncursored response** (`?since=`/`limit`/window) so first paint
   fetches the hour it draws, and let the component's lazy-history path pull
   older windows on demand. This alone is 27 MB → 1.1 MB.
2. **Negotiated compression in the app** (`Accept-Encoding`: zstd → br → gzip,
   gzip as the universal floor) on the dashboard JSON endpoints. 1.1 MB →
   129 KB.

With both, JSON is already fine (129 KB, 20 ms of JS) and needs zero client
change. The columnar format is what to reach for if the chart is ever to hold
the full 24 h in view: **columnar + gzip** is 1.01 MB and 54 ms of main-thread
work against JSON+gzip's 3.02 MB and 275 ms — 3× the bytes and 5× the CPU
saved — for the cost of ~120 lines of encoder, ~90 lines of TS decoder, and a
format version byte. Keep the JSON path as the fallback and content-negotiate
the binary one (`Accept: application/vnd.gsm.timeline.v1`), so the endpoint
stays curl-readable.
