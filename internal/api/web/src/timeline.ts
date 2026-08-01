// The dashboard's "Timeline" tab: a swimlane chart of incoming GitHub webhook
// deliveries (one lane per event type, "⇐ push") and outgoing proxied GitHub
// requests (one lane per method + route shape, "GET /repos/{owner}/{repo}/pulls"),
// rendered by the generic <timeline-view> canvas element from
// wow-look-at-my/js-snippets. Every bar/pip is a REAL measured duration —
// millisecond webhook handling renders as the component's native instant pips
// at wide zooms and re-promotes to bars when zoomed in; nothing is faked.
//
// This file is the mirror-specific adapter, packaged as the self-registering
// <gsm-timeline> custom element (the src/rate-meter.ts precedent: its own
// standalone ES module, loaded by its own <script type="module"> tag before
// app.js). app.ts creates one on the Timeline tab and keeps it ALIVE across
// the shared silent refreshes — the element polls GET /api/timeline?since=<id>
// itself (5s, paused while the page is hidden) and mergeData()s new events, so
// the canvas never suffers the other tabs' wipe-and-rebuild refresh.
//
// The component itself is NOT part of this repo: the browser imports it at
// RUNTIME from js-snippets' buildhost library site (live at master head —
// republished on every js-snippets master push; replaced the quota-dead
// GitHub Pages deploy 2026-07-20; the org's standard consumption model,
// never vendored). A failed component fetch degrades
// softly and never parks: the element shows "chart loading…" and retries the
// dynamic import on a FIXED 5s cadence forever (cache-busted ?retry=N, because
// browsers memoize failed module fetches). Fix component bugs upstream in
// js-snippets — only adapter logic lives here. Types for the URL import come
// from src/js-snippets-timeline.d.ts, an interim hand-maintained shim.

// Types only — erased at compile time. Deliberately NOT a static value import:
// a static import that fails would kill this module; the runtime load is the
// retried dynamic import in loadComponentForever below (tsc emits dynamic
// import() verbatim, so the component URL survives into the built module).
import type {
    LoadRangeFn,
    TimelineHit,
    TimelineInterval,
    TimelineLane,
    TimelineViewElement,
} from 'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js';
import type { TimelineEvent, TimelineResponse } from "./types";

const COMPONENT_URL = "https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js";
const COMPONENT_RETRY_MS = 5000; // FIXED retry cadence — never grows, never gives up
const POLL_MS = 5000; // the dashboard's shared refresh cadence (app.ts REFRESH_MS)
const FETCH_TIMEOUT_MS = 15000; // a poll that cannot settle must fail, not wedge the guard
const STALE_AFTER_MS = 12000; // ~2.5 missed polls => the chart says "stale" instead of lying
const INITIAL_WINDOW_MS = 60 * 60 * 1000; // first paint: the last hour
// The first paint FETCHES exactly the hour it draws, and older ranges arrive
// through the component's async history loader as the viewport reaches them.
// This is not a bandwidth optimization — it is what makes the frame budget
// reachable at all: a full 24h window is 100k intervals, and 100k live objects
// cost GC pauses no amount of slicing can hide (measured: 4-7 ms scavenges
// landing mid-decode). An hour is ~4k.
// A history request is clamped to this span too, so one huge zoom-out becomes
// several bounded loads rather than one 100k-interval stall; the component
// re-asks for whatever stays uncovered.
const HISTORY_SPAN_MS = 60 * 60 * 1000;
// The hard ceiling on any one synchronous stretch of decode/ingest work. A
// 60fps frame is 16.7 ms; 8 leaves half of it for the browser's own work, so
// staying under this means the chart never costs a dropped frame.
const FRAME_BUDGET_MS = 8;

// Bounded default fetcher. app.ts overrides `fetcher` only in demo mode (the
// backend-free preview serves canned data); production uses this one — the
// AbortSignal bound is what keeps the single-flight poll guard un-wedgeable.
//
// It asks for the COLUMNAR encoding (internal/api/timelinewire.go) and falls
// back to JSON on whatever the server actually answers with, so a rollback of
// the server half — or the demo preview, which serves canned JSON — keeps
// working with no branch anywhere else in this file.
async function fetchDecoded(path: string, onSlice?: (ms: number) => void): Promise<DecodedPage> {
    const res = await fetch(path, {
        headers: { Accept: `${WIRE_TYPE}, application/json;q=0.9` },
        credentials: "same-origin",
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (!res.ok) throw new Error("HTTP " + res.status);
    if ((res.headers.get("Content-Type") ?? "").startsWith(WIRE_TYPE)) {
        const buf = new Uint8Array(await res.arrayBuffer());
        return runSliced(pageColumnsGen(buf), onSlice);
    }
    return runSliced(pageFromJSONGen((await res.json()) as TimelineResponse), onSlice);
}

// Tiny DOM helper (this module is standalone; app.ts's el() is not shared).
function elem(tag: string, className: string, text?: string): HTMLElement {
    const e = document.createElement(tag);
    e.className = className;
    if (text !== undefined) e.textContent = text;
    return e;
}

function fmtDur(ms: number): string {
    if (ms < 1000) return ms + " ms";
    if (ms < 60_000) return (ms / 1000).toFixed(2) + " s";
    return (ms / 60_000).toFixed(1) + " min";
}

// ---- the wire format ----
//
// The payload is COLUMNAR (internal/api/timelinewire.go; the format and the
// numbers behind choosing it are in docs/timeline-wire-format.md): every field
// is a contiguous run, strings are a per-column dictionary plus one small
// index per event, ids and timestamps are deltas. Against the JSON it
// replaces it is ~10 B/event gzipped instead of ~32, and ~5x cheaper to turn
// into the chart's intervals — because nothing here parses per record, and
// the 19-field event object is never built at all until a tooltip asks for
// one.
//
// Decoding mirrors the encoder field for field; internal/api/timelinewire_test.go
// carries the same decoder in Go and round-trips the encoder against it, which
// is what keeps the two halves honest. A change to the layout is a NEW version
// (v2, new magic + new media type), never an edit to this one.

const WIRE_TYPE = "application/vnd.gsm.timeline.v1";
const WIRE_MAGIC = "TLC1";

// String columns, in wire order.
const COLS = ["kind", "lane", "disposition", "event_type", "action", "delivery_id",
    "repo", "method", "route", "actor", "actor_name", "detail", "target"] as const;
type ColName = (typeof COLS)[number];

interface StringColumn {
    dict: string[];
    // null when the whole window left this field empty — the encoder writes no
    // index run for such a column, and every read answers "".
    idx: Int32Array | null;
}

// One decoded page's columns. The chart holds these for as long as the
// intervals built from them are on screen.
interface Columns {
    n: number;
    id: Float64Array; // ids exceed 2^31 in a long-lived process
    start: Float64Array; // epoch ms
    dur: Int32Array;
    status: Int32Array;
    attempt: Int32Array;
    final: Uint8Array; // bitset
    s: Record<ColName, StringColumn>;
}

// An interval carries its page's COLUMNS as `data` — one shared reference for
// the whole page, not a per-row object. That is deliberate: a {columns, row}
// pair per interval is another 100k allocations, and at this scale allocation
// rate IS latency — the GC scavenges it triggers were landing inside decode
// slices as 4-7 ms pauses, which is exactly the frame budget this file exists
// to respect. The row is recovered on hover by binary-searching the id column
// (ids ascend, one page is one contiguous run), which costs ~17 comparisons
// once per tooltip.

// One decoded payload before its intervals exist.
interface DecodedPage {
    c: Columns;
    maxId: number;
    retentionStart: number; // epoch ms
    now: number; // epoch ms
}

// One chunk of built intervals, plus the lanes it introduced.
interface IntervalBatch {
    intervals: TimelineInterval[];
    lanes: Array<{ lane: string; kind: string }>;
}

interface TimelinePage {
    intervals: TimelineInterval[];
    // (lane, kind) pairs seen on this page, for the grouped lane ordering.
    lanes: Array<{ lane: string; kind: string }>;
    maxId: number;
    retentionStart: number; // epoch ms
    now: number; // epoch ms
}

// ---- cooperative slicing ----
//
// HARD REQUIREMENT: decoding a page must never block the main thread for more
// than one frame's slack. A full 24h window is ~1.8M varints and 100k
// intervals — 63 ms in one go, which is eight dropped frames — so every phase
// of the work is written as a generator that yields at chunk boundaries, and
// the driver below runs it in bounded slices, handing control back to the
// browser in between.
//
// Costs about 5% throughput against the straight-line loop. That is the point:
// a chart that takes 70 ms spread across frames beats one that takes 63 ms
// with the tab frozen.

// A resumable unit of work: yields periodically, returns its result.
type Task<T> = Generator<undefined, T, undefined>;

// Chunk sizing is ADAPTIVE, not a constant. A fixed chunk cannot satisfy the
// frame budget: 2048 rows is ~0.2 ms of varint decoding and ~3.5 ms of the
// JSON fallback's transpose, on THIS machine — a slower one shifts both. So
// each phase times its own chunks and steers toward TARGET_CHUNK_MS of work,
// which is what bounds the overshoot: the driver checks the clock only BETWEEN
// chunks, so the worst slice is its budget plus one chunk.
const TARGET_CHUNK_MS = 1;
const MIN_CHUNK = 32;
const MAX_CHUNK = 8192;
// Each phase starts SMALL and ramps. The first execution of any of these
// loops runs cold — unoptimized, uninlined — and is several times slower than
// the steady state that follows, so a phase that opened at its steady-state
// size would blow the budget exactly once, on the slice that matters most (the
// first paint). Growth is capped per step for the same reason: one unusually
// cheap chunk must not license a chunk 50x larger.
const START_CHUNK = 32;
const MAX_GROWTH = 4;

function nextChunk(size: number, ms: number, targetMs = TARGET_CHUNK_MS,
                   max = MAX_CHUNK): number {
    const ceiling = Math.min(size * MAX_GROWTH, max);
    if (ms <= 0) return ceiling;
    const scaled = Math.round((size * targetMs) / ms);
    return Math.max(MIN_CHUNK, Math.min(ceiling, scaled || MIN_CHUNK));
}

// Handing intervals to the component is main-thread work too — it ingests,
// re-packs sub-tracks and marks the canvas dirty — and it is NOT ours to
// slice, so the element yields around every call and never runs a decode slice
// and a feed in the same task (that combination measured 25 ms in a browser,
// and the node harness cannot see it because it has no component).
//
// The feed SIZE trades two costs against each other: measured in a real
// browser one call runs ~2.8 us per interval (512 -> ~1.8 ms, 2048 -> ~5.5 ms),
// so smaller feeds keep each task shorter — but every call marks the canvas
// dirty, so smaller feeds also mean more renders for the same data. A fixed
// 1024 measured 7.6 ms on a loaded machine: too close to the budget to be a
// constant. So it is steered by what the last call actually cost, which lets a
// fast machine take big feeds (few renders) and a slow one take small ones
// (short tasks) without either blowing the budget.
// The CAP matters as much as the target, because the cost is not linear in the
// feed alone: a merge gets dearer as the chart holds more, so an estimate from
// an early cheap call overshoots later ones (measured: the size ran up to 2048
// and those calls then cost 5.5-6.0 ms). 512 is the largest size measured to
// stay comfortably inside the budget once the chart is full, and the adaptive
// size still shrinks below it on a slower machine. The first call is smaller
// again: it is the one that initializes the canvas.
const TARGET_FEED_MS = 2;
const MAX_FEED_INTERVALS = 512;
const START_FEED_INTERVALS = 128;

// SLICE_BUDGET_MS is how long a single task may run before yielding. The
// requirement is 8 ms per frame; 3 leaves room for the yield itself, for the
// component's own render, and for a chunk that overruns because the machine is
// slow — the clock is only checked BETWEEN chunks, so the true bound is
// budget + one chunk.
const SLICE_BUDGET_MS = 3;

// yieldToBrowser hands control back so the frame can render. scheduler.yield()
// is the purpose-built API (and keeps this work's continuation prioritized
// ahead of new tasks); the MessageChannel fallback is a real macrotask without
// setTimeout's 4 ms clamping.
function yieldToBrowser(): Promise<void> {
    const sched = (globalThis as { scheduler?: { yield?: () => Promise<void> } }).scheduler;
    if (typeof sched?.yield === "function") return sched.yield();
    return new Promise<void>((resolve) => {
        const ch = new MessageChannel();
        ch.port1.onmessage = () => {
            ch.port1.close();
            resolve();
        };
        ch.port2.postMessage(0);
    });
}

// yieldToNextFrame waits for the browser to actually PAINT, then resumes in a
// fresh task. yieldToBrowser above only yields the task — several yields can
// still land in one frame, which is right for decode work but wrong for
// feeding the chart: every feed makes the component redraw, so N feeds in one
// frame means it draws the whole (already grown) chart in one go. Paced one
// per frame, each draw is a small increment on a chart that grew a little,
// which is what keeps the first paint inside the frame budget.
function yieldToNextFrame(): Promise<void> {
    if (typeof requestAnimationFrame !== "function") return yieldToBrowser();
    return new Promise<void>((resolve) => {
        requestAnimationFrame(() => setTimeout(resolve, 0));
    });
}

// runSliced drives a task to completion, never running it for more than
// SLICE_BUDGET_MS at a stretch. onSlice reports each slice's real duration —
// the element uses it to assert the frame budget in dev, and the bench to
// measure the worst block.
export async function runSliced<T>(task: Task<T>, onSlice?: (ms: number) => void): Promise<T> {
    for (;;) {
        const t0 = performance.now();
        let r = task.next();
        while (!r.done && performance.now() - t0 < SLICE_BUDGET_MS) r = task.next();
        onSlice?.(performance.now() - t0);
        if (r.done) return r.value;
        await yieldToBrowser();
    }
}

// drain runs a task straight through, for callers that are not on a frame
// budget: tests, and payloads small enough that the whole thing is one chunk.
function drain<T>(task: Task<T>): T {
    let r = task.next();
    while (!r.done) r = task.next();
    return r.value;
}

class WireReader {
    private p = 0;
    private dec = new TextDecoder();
    constructor(private b: Uint8Array) {}

    // Varints are read as floats past 2^31 (ids and epoch-ms deltas both
    // exceed it); 2**s keeps the shift exact instead of wrapping at 32 bits.
    uvarint(): number {
        let x = 0, s = 0;
        for (;;) {
            const c = this.b[this.p++];
            if (c === undefined) throw new Error("timeline: truncated varint");
            if (c < 0x80) return x + c * 2 ** s;
            x += (c & 0x7f) * 2 ** s;
            s += 7;
        }
    }

    varint(): number { // zigzag
        const u = this.uvarint();
        return u % 2 === 0 ? u / 2 : -(u + 1) / 2;
    }

    // Bulk readers for the columns. Two properties matter here:
    //
    //  - Nearly every value on the wire fits in one byte (a dictionary index
    //    for a column with <128 entries, a 1-per-event id delta, a 3 ms
    //    duration), so the single-byte case is inlined rather than paying a
    //    call into uvarint() ~1.8M times per full window.
    //  - Each reader decodes a ROW RANGE, not a whole column, and the read
    //    position lives on the reader. That is what makes a decode
    //    interruptible: the sliced driver calls these in chunks and yields to
    //    the browser between them, so no single task blocks a frame.
    uvarints(out: Int32Array | Float64Array, from: number, to: number): void {
        const b = this.b;
        let p = this.p;
        for (let i = from; i < to; i++) {
            const c0 = b[p++];
            if (c0 < 0x80) {
                out[i] = c0;
                continue;
            }
            let x = c0 & 0x7f, sh = 7;
            for (;;) {
                const c = b[p++];
                if (c === undefined) throw new Error("timeline: truncated varint");
                if (c < 0x80) {
                    x += c * 2 ** sh;
                    break;
                }
                x += (c & 0x7f) * 2 ** sh;
                sh += 7;
            }
            out[i] = x;
        }
        this.p = p;
    }

    // Running sums of zigzag deltas, straight into the output column. The
    // accumulator is the previous value already written, so a range resumes
    // exactly where the last one stopped.
    varintSums(out: Float64Array, from: number, to: number): void {
        const b = this.b;
        let p = this.p, acc = from === 0 ? 0 : out[from - 1];
        for (let i = from; i < to; i++) {
            let u = b[p++];
            if (u >= 0x80) {
                u &= 0x7f;
                let sh = 7;
                for (;;) {
                    const c = b[p++];
                    if (c === undefined) throw new Error("timeline: truncated varint");
                    if (c < 0x80) {
                        u += c * 2 ** sh;
                        break;
                    }
                    u += (c & 0x7f) * 2 ** sh;
                    sh += 7;
                }
            }
            acc += u % 2 === 0 ? u / 2 : -(u + 1) / 2;
            out[i] = acc;
        }
        this.p = p;
    }

    // Running sums of unsigned deltas (ids), same resumption rule.
    uvarintSums(out: Float64Array, from: number, to: number): void {
        const b = this.b;
        let p = this.p, acc = from === 0 ? 0 : out[from - 1];
        for (let i = from; i < to; i++) {
            const c0 = b[p++];
            if (c0 < 0x80) {
                out[i] = acc += c0;
                continue;
            }
            let x = c0 & 0x7f, sh = 7;
            for (;;) {
                const c = b[p++];
                if (c === undefined) throw new Error("timeline: truncated varint");
                if (c < 0x80) {
                    x += c * 2 ** sh;
                    break;
                }
                x += (c & 0x7f) * 2 ** sh;
                sh += 7;
            }
            out[i] = acc += x;
        }
        this.p = p;
    }

    bytes(n: number): Uint8Array {
        if (this.p + n > this.b.length) throw new Error("timeline: truncated payload");
        const out = this.b.subarray(this.p, this.p + n);
        this.p += n;
        return out;
    }

    str(): string {
        return this.dec.decode(this.bytes(this.uvarint()));
    }

    // A dictionary can be large (delivery GUIDs are unique per webhook), so
    // reading one is chunked like everything else.
    *dictChunked(): Task<string[]> {
        const n = this.uvarint();
        const out = new Array<string>(n);
        let size = START_CHUNK;
        for (let i = 0; i < n; ) {
            const to = Math.min(i + size, n);
            const t0 = performance.now();
            for (; i < to; i++) out[i] = this.str();
            size = nextChunk(size, performance.now() - t0);
            yield;
        }
        return out;
    }

    expectMagic(want: string): void {
        const got = String.fromCharCode(...this.bytes(4));
        if (got !== want) throw new Error(`timeline: bad magic ${JSON.stringify(got)}`);
    }

    atEnd(): boolean {
        return this.p === this.b.length;
    }
}

// pageColumnsGen decodes the payload's preamble and columns, yielding between
// chunks. The intervals are built separately (intervalsGen) so the element can
// paint the first batch before the last one exists.
export function* pageColumnsGen(buf: Uint8Array): Task<DecodedPage> {
    const r = new WireReader(buf);
    r.expectMagic(WIRE_MAGIC);
    const maxId = r.uvarint();
    const retentionStart = r.varint();
    const now = r.varint();
    const n = r.uvarint();

    const c: Columns = {
        n,
        id: new Float64Array(n),
        start: new Float64Array(n),
        dur: new Int32Array(n),
        status: new Int32Array(n),
        attempt: new Int32Array(n),
        final: new Uint8Array(0),
        s: {} as Record<ColName, StringColumn>,
    };
    yield* chunked(n, (from, to) => r.uvarintSums(c.id, from, to));
    yield* chunked(n, (from, to) => r.varintSums(c.start, from, to));
    yield* chunked(n, (from, to) => r.uvarints(c.dur, from, to));
    yield* chunked(n, (from, to) => r.uvarints(c.status, from, to));
    yield* chunked(n, (from, to) => r.uvarints(c.attempt, from, to));
    c.final = r.bytes((n + 7) >> 3);

    for (const name of COLS) {
        const dict = yield* r.dictChunked();
        if (dict.length === 1) {
            c.s[name] = { dict, idx: null }; // column unused in this window
            continue;
        }
        const idx = new Int32Array(n);
        yield* chunked(n, (from, to) => r.uvarints(idx, from, to));
        c.s[name] = { dict, idx };
    }
    if (!r.atEnd()) throw new Error("timeline: trailing bytes in payload");
    return { c, maxId, retentionStart, now };
}

// chunked walks [0,n) in adaptively-sized ranges, yielding after each.
function* chunked(n: number, work: (from: number, to: number) => void): Task<void> {
    let size = START_CHUNK;
    for (let i = 0; i < n; ) {
        const to = Math.min(i + size, n);
        const t0 = performance.now();
        work(i, to);
        size = nextChunk(size, performance.now() - t0);
        i = to;
        yield;
    }
}

// The JSON fallback lands in the SAME column representation, so everything
// downstream — intervals, lanes, tooltips — has exactly one shape to handle.
// It is sliced too: JSON.parse itself is one unbreakable native call, but the
// 100k-row transpose after it is ours and must not block either.
export function* pageFromJSONGen(resp: TimelineResponse): Task<DecodedPage> {
    const events = resp.events ?? [];
    const n = events.length;
    const c: Columns = {
        n,
        id: new Float64Array(n),
        start: new Float64Array(n),
        dur: new Int32Array(n),
        status: new Int32Array(n),
        attempt: new Int32Array(n),
        final: new Uint8Array((n + 7) >> 3),
        s: {} as Record<ColName, StringColumn>,
    };
    const dicts = new Map<ColName, { dict: string[]; seen: Map<string, number>; idx: Int32Array }>();
    for (const name of COLS) {
        dicts.set(name, { dict: [""], seen: new Map([["", 0]]), idx: new Int32Array(n) });
    }
    yield* chunked(n, (from, to) => {
        for (let i = from; i < to; i++) {
            const e = events[i];
            c.id[i] = e.id;
            c.start[i] = Date.parse(e.start);
            c.dur[i] = e.dur_ms;
            c.status[i] = e.status ?? 0;
            c.attempt[i] = e.attempt ?? 0;
            if (e.final) c.final[i >> 3] |= 1 << (i & 7);
            for (const name of COLS) {
                const v = (e as unknown as Record<string, unknown>)[name];
                const value = typeof v === "string" ? v : "";
                const d = dicts.get(name)!;
                let ix = d.seen.get(value);
                if (ix === undefined) {
                    ix = d.dict.length;
                    d.dict.push(value);
                    d.seen.set(value, ix);
                }
                d.idx[i] = ix;
            }
        }
    });
    for (const name of COLS) {
        const d = dicts.get(name)!;
        c.s[name] = { dict: d.dict, idx: d.dict.length === 1 ? null : d.idx };
    }
    return {
        c,
        maxId: resp.max_id,
        retentionStart: Date.parse(resp.retention_start),
        now: Date.parse(resp.now),
    };
}

// Synchronous convenience wrappers: the Go/node cross-decode test and any
// caller not on a frame budget. The element never uses these.
export function pageFromWire(buf: Uint8Array): TimelinePage {
    return wholePage(drain(pageColumnsGen(buf)));
}

export function pageFromJSON(resp: TimelineResponse): TimelinePage {
    return wholePage(drain(pageFromJSONGen(resp)));
}

function wholePage(d: DecodedPage): TimelinePage {
    const batches: IntervalBatch[] = [];
    drain(intervalsGen(d.c, (b) => batches.push(b)));
    return {
        intervals: batches.flatMap((b) => b.intervals),
        lanes: mergeLaneBatches(batches),
        maxId: d.maxId,
        retentionStart: d.retentionStart,
        now: d.now,
    };
}

function mergeLaneBatches(batches: IntervalBatch[]): Array<{ lane: string; kind: string }> {
    const lanes = new Map<string, string>();
    for (const b of batches) {
        for (const l of b.lanes) if (!lanes.has(l.lane)) lanes.set(l.lane, l.kind);
    }
    return [...lanes].map(([lane, kind]) => ({ lane, kind }));
}

const str = (col: StringColumn, i: number): string => (col.idx === null ? "" : col.dict[col.idx[i]]);

// ---- columns → component translation ----

// Per-row label and state are pure functions of a few dictionary INDICES and
// the status, so they are resolved once per dictionary ENTRY (a handful) and
// then read per row (100k). This is what keeps the hot loop below to typed-
// array reads plus array indexing — no string comparisons, no per-row branch
// on kind.

// stateFor one (kind, disposition) pair — the chart's failed/dim marking.
function stateForPair(kind: string, disp: string): string {
    if (kind === "webhook") {
        // error = dispatch failed; unverified/rejected/unparseable = a
        // delivery refused before dispatch — all unmissable.
        if (disp === "error" || disp === "unverified" ||
            disp === "rejected" || disp === "unparseable") return "failed";
        if (disp === "ignored") return "dim"; // received but not tracked
        return "";
    }
    if (kind === "notify") return disp === "failed" ? "failed" : "";
    // Requests/exchanges: a failed exchange is unmissable; the 5xx half of the
    // rule is per-row (it reads the status) and is applied in the loop.
    return disp === "error" ? "failed" : "";
}

// intervalsGen builds intervals in BATCHES, handing each to emit as soon as it
// is ready — and NOTHING else per row. The 19-field event object a tooltip
// needs is built on hover (eventAt), which is most of why this outruns the
// JSON path it replaced: 100k intervals now, one event object when someone
// points at one. Batching is what lets the element show the newest slice of
// the chart while the rest is still decoding.
export function* intervalsGen(c: Columns, emit: (batch: IntervalBatch) => void): Task<void> {
    const kindC = c.s.kind, laneC = c.s.lane, dispC = c.s.disposition,
        repoC = c.s.repo, actionC = c.s.action;
    const kindIdx = kindC.idx, laneIdx = laneC.idx, dispIdx = dispC.idx,
        repoIdx = repoC.idx, actionIdx = actionC.idx;
    const { start: startCol, dur: durCol, status: statusCol, id: idCol } = c;

    // state per (kind, disposition) pair — both dictionaries are tiny.
    const stateTable: string[][] = kindC.dict.map((kind) =>
        dispC.dict.map((disp) => stateForPair(kind, disp)));
    // A webhook's label is its repo NAME; a request's is its status. Both
    // resolve per dictionary entry, not per row.
    const isWebhook = kindC.dict.map((k) => k === "webhook");
    const repoLabel = repoC.dict.map((r) => (r ? r.slice(r.indexOf("/") + 1) : ""));
    const statusLabel = new Map<number, string>();
    const labelForStatus = (st: number): string => {
        if (!st) return "";
        let s = statusLabel.get(st);
        if (s === undefined) statusLabel.set(st, (s = String(st)));
        return s;
    };

    let size = START_CHUNK;
    for (let from = 0; from < c.n; ) {
        const to = Math.min(from + size, c.n);
        const t0 = performance.now();
        const intervals = new Array<TimelineInterval>(to - from);
        const lanes = new Map<string, string>();
        for (let i = from; i < to; i++) {
            const ki = kindIdx === null ? 0 : kindIdx[i];
            const lane = laneIdx === null ? "" : laneC.dict[laneIdx[i]];
            if (!lanes.has(lane)) lanes.set(lane, kindC.dict[ki]);

            const status = statusCol[i];
            let label: string;
            if (isWebhook[ki]) {
                const rl = repoIdx === null ? "" : repoLabel[repoIdx[i]];
                label = rl || (actionIdx === null ? "" : actionC.dict[actionIdx[i]]);
            } else {
                label = labelForStatus(status);
            }
            let state = dispIdx === null ? "" : stateTable[ki][dispIdx[i]];
            if (!state && !isWebhook[ki] && status >= 500) state = "failed";

            const start = startCol[i];
            intervals[i - from] = {
                id: String(idCol[i]),
                laneId: lane,
                start,
                // end = start + the REAL measured duration. Never null (every
                // recorded event finished) and never inflated: a sub-pixel
                // span is the component's native instant pip, which is exactly
                // right for a 3ms webhook dispatch.
                end: start + (durCol[i] > 0 ? durCol[i] : 0),
                label,
                category: lane, // stable hue per lane
                state,
                data: c,
            };
        }
        size = nextChunk(size, performance.now() - t0);
        emit({ intervals, lanes: [...lanes].map(([lane, kind]) => ({ lane, kind })) });
        from = to;
        yield;
    }
}

// laneKindsOf reads the page's COMPLETE lane set straight off the columns —
// every lane is a dictionary entry, and one row per lane gives its kind — so
// the chart can be told about all of them on the first feed.
//
// That matters for smoothness, not tidiness: a lane arriving mid-stream is a
// STRUCTURAL change to the component, which forces it to re-cluster and
// re-assign every lane it holds. Discovering lanes batch by batch therefore
// turned each batch into whole-chart work; declaring them up front leaves the
// component free to do only what changed.
export function laneKindsOf(c: Columns): Array<{ lane: string; kind: string }> {
    const laneCol = c.s.lane, kindCol = c.s.kind;
    const dict = laneCol.dict;
    const out: Array<{ lane: string; kind: string }> = [];
    if (laneCol.idx === null) return out;
    const kindByLaneIdx = new Map<number, string>();
    for (let i = 0; i < c.n && kindByLaneIdx.size < dict.length - 1; i++) {
        const li = laneCol.idx[i];
        if (li !== 0 && !kindByLaneIdx.has(li)) kindByLaneIdx.set(li, str(kindCol, i));
    }
    for (const [li, kind] of kindByLaneIdx) out.push({ lane: dict[li], kind });
    return out;
}

// eventAt materializes one row as the flat event the tooltip reads. Called
// once per hover, never per row.
export function rowOfEventId(c: Columns, id: number): number {
    let lo = 0, hi = c.n - 1;
    while (lo <= hi) {
        const mid = (lo + hi) >> 1;
        const v = c.id[mid];
        if (v === id) return mid;
        if (v < id) lo = mid + 1;
        else hi = mid - 1;
    }
    return -1;
}

export function eventAt(c: Columns, i: number): TimelineEvent {
    return {
        id: c.id[i],
        kind: str(c.s.kind, i),
        lane: str(c.s.lane, i),
        start: new Date(c.start[i]).toISOString(),
        dur_ms: c.dur[i],
        disposition: str(c.s.disposition, i),
        event_type: str(c.s.event_type, i),
        action: str(c.s.action, i),
        delivery_id: str(c.s.delivery_id, i),
        repo: str(c.s.repo, i),
        method: str(c.s.method, i),
        route: str(c.s.route, i),
        status: c.status[i],
        actor: str(c.s.actor, i),
        actor_name: str(c.s.actor_name, i),
        detail: str(c.s.detail, i),
        target: str(c.s.target, i),
        attempt: c.attempt[i],
        final: (c.final[i >> 3] & (1 << (i & 7))) !== 0,
    };
}

function tooltipFor(hit: TimelineHit): Node | null {
    if (hit.type !== "interval" || !hit.interval) return null;
    const c = hit.interval.data as Columns | undefined;
    if (!c?.n) return null;
    // (`row` is taken below by the tooltip's line helper.)
    const rowIdx = rowOfEventId(c, Number(hit.interval.id));
    if (rowIdx < 0) return null;
    const e = eventAt(c, rowIdx);
    const root = document.createElement("div");
    const row = (k: string, v: string): void => {
        if (v === "") return;
        const r = elem("div", "tt-row");
        r.appendChild(elem("span", "tt-k", k));
        r.appendChild(elem("span", "tt-v", v));
        root.appendChild(r);
    };
    if (e.kind === "webhook") {
        root.appendChild(elem("div", "tt-title", e.event_type
            ? "⇐ " + e.event_type + (e.action ? "." + e.action : "")
            : e.lane));
        row("repo", e.repo ?? "");
        row("delivery", e.delivery_id ?? "");
        row("disposition", e.disposition ?? "");
        row("detail", e.detail ?? "");
    } else if (e.kind === "notify") {
        root.appendChild(elem("div", "tt-title", e.lane));
        row("target", e.target ?? "");
        row("status", e.status ? String(e.status) : "");
        row("attempt", e.attempt ? String(e.attempt) + (e.final ? " (final)" : "") : "");
        row("disposition", e.disposition ?? "");
    } else {
        root.appendChild(elem("div", "tt-title", (e.method ?? "") + " " + (e.route ?? "")));
        row("status", e.status ? String(e.status) : "");
        row("actor", e.actor_name ? e.actor_name + " (" + (e.actor ?? "") + ")" : (e.actor ?? ""));
        row("disposition", e.disposition ?? "");
        row("detail", e.detail ?? "");
    }
    row("duration", fmtDur(e.dur_ms));
    row("at", new Date(Date.parse(e.start)).toISOString());
    return root;
}

// ---- the <gsm-timeline> element ----

class GsmTimeline extends HTMLElement {
    // Overridable data source (demo mode); see fetchTimeline.
    fetcher: ((path: string) => Promise<TimelineResponse>) | null = null;

    private view: TimelineViewElement | null = null;
    private note: HTMLElement | null = null;
    private timer: ReturnType<typeof setInterval> | undefined;
    private pollInFlight = false;
    private maxId = 0;
    // The ring's retention floor (epoch ms) as of the last read — the point at
    // which history is genuinely exhausted rather than merely unfetched.
    private retentionStart = 0;
    // Oldest instant the chart has data for; the live path's coverage floor.
    private coveredFrom = 0;
    private seeded = false;
    // laneId -> kind, for the grouped lane ordering (webhooks first).
    private laneKinds = new Map<string, string>();
    private laneOrderKey = "";
    // Longest synchronous slice this element has run, exposed for the bench
    // and for anyone wondering whether the frame budget is being met.
    worstSliceMs = 0;
    // How many intervals to hand the component per call; steered by what the
    // last call actually cost (see TARGET_FEED_MS).
    private feedSize = START_FEED_INTERVALS;

    connectedCallback(): void {
        if (this.view || this.note) {
            // Reconnect of an already-booted instance (app.ts never moves the
            // element, but be correct anyway): resume the poll the disconnect
            // stopped.
            this.startPolling();
            return;
        }
        this.note = elem("p", "timeline-note", "chart loading…");
        this.appendChild(this.note);
        void this.boot();
    }

    disconnectedCallback(): void {
        // The tab was left (loadView wipes #scope-body): stop polling. A fresh
        // element is created on the next visit; this one is garbage.
        if (this.timer !== undefined) clearInterval(this.timer);
        this.timer = undefined;
    }

    private async boot(): Promise<void> {
        if (!(await this.loadComponentForever())) return; // disconnected while retrying
        const tl = document.createElement("timeline-view") as TimelineViewElement;
        tl.tooltipFor = tooltipFor;
        // Async history: the chart asks for older ranges as the viewport
        // reaches them, instead of the page paying for 24h up front.
        // Feature-detected so an older component build still renders.
        if ("loadRange" in tl) tl.loadRange = this.loadRange;
        // Feature-detected staleness marking: with a 5s poll, ~2.5 missed
        // polls means the feed is genuinely dead — say so on the chart
        // instead of extrapolating.
        if (typeof tl.markFresh === "function") tl.staleAfterMs = STALE_AFTER_MS;
        tl.setAttribute("empty-text", "no traffic recorded yet");
        this.view = tl;
        this.appendChild(tl);
        // Let the EMPTY chart paint once before any data arrives. That first
        // paint is where the cold costs live — font metrics for the axis and
        // gutter, the canvas backing store, the component's own warm-up — and
        // paying them on an empty frame keeps them out of the frame that also
        // ingests and draws real data. In production the network round-trip
        // usually provides this gap; a fast (or cached) response does not.
        await yieldToNextFrame();
        await this.poll(); // first page: setData + initial viewport
        this.note?.remove();
        this.note = null;
        this.startPolling();
    }

    private startPolling(): void {
        if (this.timer !== undefined || !this.isConnected || !this.view) return;
        this.timer = setInterval(() => {
            if (document.hidden) return;
            void this.poll();
        }, POLL_MS);
    }

    // Dynamic-import the Pages component, retrying forever on a fixed cadence
    // (cache-busted — browsers memoize failed module fetches). Bails out only
    // if the element left the DOM (tab switched away mid-retry).
    private async loadComponentForever(): Promise<boolean> {
        for (let attempt = 0; ; attempt++) {
            if (!this.isConnected) return false;
            try {
                await import(attempt === 0 ? COMPONENT_URL : `${COMPONENT_URL}?retry=${attempt}`);
                return true;
            } catch (e) {
                console.error(`timeline: component load failed (retry in ${COMPONENT_RETRY_MS}ms):`, e);
                await new Promise((r) => setTimeout(r, COMPONENT_RETRY_MS));
            }
        }
    }

    private async poll(): Promise<void> {
        const tl = this.view;
        if (!tl || this.pollInFlight) return;
        this.pollInFlight = true;
        try {
            // First read: the hour the chart paints. After that the live
            // cursor, which returns only what is new (a handful of events).
            let path: string;
            if (this.maxId > 0) {
                path = "/api/timeline?since=" + this.maxId;
            } else {
                this.coveredFrom = Date.now() - INITIAL_WINDOW_MS;
                path = "/api/timeline?from=" + this.coveredFrom;
            }
            // A demo-mode override serves canned JSON; production takes the
            // negotiated (columnar) path. Both decode into the same columns,
            // in slices, so neither can freeze the tab on a full window.
            const fetcher = this.fetcher;
            const decoded = fetcher
                ? await runSliced(pageFromJSONGen(await fetcher(path)), this.recordSlice)
                : await fetchDecoded(path, this.recordSlice);
            if (!this.isConnected || this.view !== tl) return; // tab left mid-decode
            if (decoded.maxId > this.maxId) this.maxId = decoded.maxId;
            this.retentionStart = decoded.retentionStart;
            // The live read covers [what it asked for, now]. On the first poll
            // that is the initial window; afterwards the cursor's events all
            // postdate it, so the covered end simply advances.
            await this.merge(tl, decoded, {
                start: this.coveredFrom || decoded.now - INITIAL_WINDOW_MS,
                end: decoded.now,
            });
        } catch (e) {
            // Keep the last data; the component's staleness marking says the
            // feed is down once STALE_AFTER_MS passes without a markFresh.
            console.error("timeline: poll failed:", e);
        } finally {
            this.pollInFlight = false;
        }
    }

    // recordSlice enforces the frame budget in the only place that can see it.
    // A slice that overruns is a real defect (a chunk that got too expensive,
    // or a phase that forgot to yield) and says so in the console rather than
    // silently dropping frames.
    private readonly recordSlice = (ms: number): void => {
        if (ms > this.worstSliceMs) this.worstSliceMs = ms;
        if (ms > FRAME_BUDGET_MS) {
            console.warn(`timeline: decode slice ran ${ms.toFixed(1)}ms (budget ${FRAME_BUDGET_MS}ms)`);
        }
    };

    // Feeds the chart in BATCHES, yielding between them: the first batch
    // paints (and sets the viewport) while the rest is still being built, and
    // no single setData/mergeData call ever sees 100k intervals — the
    // component's own ingestion is main-thread work too.
    // merge feeds one decoded page into the chart. Used by BOTH paths — the
    // live poll and the history loader — which differ only in what they cover
    // and whether they advance the live cursor (the caller decides; see
    // loadRange).
    private async merge(tl: TimelineViewElement, page: DecodedPage,
                        coverage: { start: number; end: number }): Promise<void> {

        // Declare every lane this page carries BEFORE the first feed: a lane
        // appearing later is a structural change that costs the component a
        // whole-chart relayout (see laneKindsOf).
        for (const { lane, kind } of laneKindsOf(page.c)) {
            if (!this.laneKinds.has(lane)) this.laneKinds.set(lane, kind);
        }

        const pending: IntervalBatch[] = [];
        const task = intervalsGen(page.c, (b) => pending.push(b));
        let empty = true;
        for (;;) {
            const t0 = performance.now();
            let r = task.next();
            while (!r.done && performance.now() - t0 < SLICE_BUDGET_MS) r = task.next();
            this.recordSlice(performance.now() - t0);

            for (const batch of pending.splice(0)) {
                if (!batch.intervals.length) continue;
                empty = false;
                for (const { lane, kind } of batch.lanes) {
                    if (!this.laneKinds.has(lane)) this.laneKinds.set(lane, kind);
                }
                for (let i = 0; i < batch.intervals.length; ) {
                    await yieldToNextFrame();
                    if (!this.isConnected || this.view !== tl) return; // tab left mid-feed
                    const part = batch.intervals.slice(i, i + this.feedSize);
                    i += part.length;
                    const t1 = performance.now();
                    this.feed(tl, part, coverage);
                    const ms = performance.now() - t1;
                    this.feedSize = nextChunk(this.feedSize, ms, TARGET_FEED_MS, MAX_FEED_INTERVALS);
                    this.recordSlice(ms);
                }
            }
            if (r.done) break;
            await yieldToBrowser();
            if (!this.isConnected || this.view !== tl) return; // tab left mid-apply
        }

        // An empty poll is still proof the feed is alive — and the very first
        // one still has to seed the chart, or an idle mirror renders nothing.
        if (empty && !this.seeded) {
            this.seeded = true;
            const lanes = this.computeLanes();
            this.laneOrderKey = lanes.map((l) => l.id).join("\n");
            tl.setData({ lanes, intervals: [], coverage });
            const now = coverage.end || Date.now();
            tl.setViewport(now - INITIAL_WINDOW_MS, now);
            tl.followNow = true;
        }
        this.markFresh(tl);
    }

    // loadRange answers the component's history requests. It is clamped to
    // HISTORY_SPAN_MS per call and merged in the same sliced batches as a
    // live poll, so panning into the past can never cost a frame either.
    private readonly loadRange: LoadRangeFn = async (start, end) => {
        const tl = this.view;
        if (!tl) return;
        const floor = this.retentionStart;
        if (floor > 0 && end <= floor) return { exhausted: true };
        const from = Math.max(Math.round(start), Math.round(end) - HISTORY_SPAN_MS, floor);
        if (from >= end) return { exhausted: true };
        const decoded = await fetchDecoded(
            `/api/timeline?from=${from}&to=${Math.round(end)}`, this.recordSlice);
        if (!this.isConnected || this.view !== tl) return;
        // History NEVER advances the live cursor: this response was assembled
        // from an older window, and adopting its max_id would skip every event
        // recorded between the last live poll and now.
        await this.merge(tl, decoded, { start: from, end: Math.round(end) });
        if (this.coveredFrom === 0 || from < this.coveredFrom) this.coveredFrom = from;
        return from <= this.retentionStart ? { exhausted: true } : undefined;
    };

    // feed hands ONE bounded batch to the component: the first call seeds the
    // chart (and the viewport), every later one merges — never setData, which
    // would wipe held data and reset sub-track packing mid-view.
    private feed(tl: TimelineViewElement, intervals: TimelineInterval[],
                 coverage: { start: number; end: number }): void {
        if (!this.seeded) {
            this.seeded = true;
            this.laneOrderKey = "";
            const lanes = this.computeLanes();
            this.laneOrderKey = lanes.map((l) => l.id).join("\n");
            tl.setData({ lanes, intervals, coverage });
            const now = coverage.end || Date.now();
            tl.setViewport(now - INITIAL_WINDOW_MS, now);
            tl.followNow = true;
            return;
        }
        // Coverage rides along so the live window keeps tracking now.
        tl.mergeData({ intervals, coverage });
        this.syncLanes(tl); // no-op unless the lane ORDER actually changed
    }

    private markFresh(tl: TimelineViewElement): void {
        if (typeof tl.markFresh === "function") tl.markFresh();
    }

    // Lane order: webhook lanes first, then request/exchange lanes, then the
    // outbound notify lane — each group alphabetical: deterministic and
    // viewport-independent (the webhook-runner precedent: activity-based
    // ordering makes rows jump around).
    private computeLanes(): TimelineLane[] {
        const webhooks: string[] = [];
        const requests: string[] = [];
        const notify: string[] = [];
        for (const [id, kind] of this.laneKinds) {
            if (kind === "webhook") webhooks.push(id);
            else if (kind === "notify") notify.push(id);
            else requests.push(id);
        }
        webhooks.sort();
        requests.sort();
        notify.sort();
        return [...webhooks, ...requests, ...notify].map((id) => ({ id, label: id }));
    }

    // Re-apply lane order only when it actually changed (setLanes re-ingests).
    private syncLanes(tl: TimelineViewElement): void {
        const lanes = this.computeLanes();
        const key = lanes.map((l) => l.id).join("\n");
        if (key === this.laneOrderKey) return;
        this.laneOrderKey = key;
        tl.setLanes(lanes);
    }
}

customElements.define("gsm-timeline", GsmTimeline);
