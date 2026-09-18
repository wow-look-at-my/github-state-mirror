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
// The WIRE FORMAT lives with the timeline too: it exists to feed that chart,
// so js-snippets owns the codec and the frame-paced driver. What stays here is
// this mirror's vocabulary — which columns it sends, and what its events MEAN
// (labels, states, lanes). The library decodes a layout; it has never heard of
// a delivery id.
import type {
    Columns,
    DecodedPage,
    StringColumn,
    Task,
    WireSchema,
} from 'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-wire.js';
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

// Bounded default fetcher. app.ts overrides `fetcher` only in demo mode (the
// backend-free preview serves canned data); production uses this one — the
// AbortSignal bound is what keeps the single-flight poll guard un-wedgeable.
//
// This asks for the columnar encoding and ACCEPTS NOTHING ELSE: a content type
// it does not recognize THROWS, surfacing as the chart's "feed is down" state.
//
// That strictness is load-bearing, not defensive tidiness. The endpoint also
// serves readable JSON to callers that do not name the wire type (curl, an
// operator with jq), and this is what keeps that harmless: an earlier version
// FELL BACK to whatever came back, so an Accept that drifted quietly took a
// decode costing ~10x the frames with nothing failing. The defect was the
// fallback, not the JSON. TestTimelineClientRefusesJSON pins this.
export async function fetchTimelineBytes(path: string): Promise<Uint8Array> {
    const res = await fetch(path, {
        headers: { Accept: WIRE_TYPE },
        credentials: "same-origin",
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (!res.ok) throw new Error("HTTP " + res.status);
    const ct = res.headers.get("Content-Type") ?? "";
    if (!ct.startsWith(WIRE_TYPE)) throw new Error("timeline: expected " + WIRE_TYPE + ", got " + ct);
    return new Uint8Array(await res.arrayBuffer());
}

/** Fetch + decode. Split from the fetch above so the REFUSAL can be tested
 *  without the library loaded — see TestTimelineClientRefusesJSON. */
export async function fetchDecoded(path: string, onSlice?: (ms: number) => void): Promise<DecodedPage> {
    const buf = await fetchTimelineBytes(path);
    return w().runSliced(w().decodePageGen(buf, SCHEMA), onSlice);
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
// The payload is COLUMNAR, and both halves of that format live elsewhere: the
// Go encoder in internal/api/timelinewire.go, the decoder + frame-paced driver
// in js-snippets (ui/timeline-wire.js), imported at RUNTIME like the component
// beside it. Against the JSON it replaces it is ~10 B/event gzipped instead of
// ~32, and ~5x cheaper to turn into intervals.
//
// The two implementations are pinned to each other by BYTES, not by running
// one from the other: internal/api/timelinewire_golden_test.go asserts the
// encoder emits an exact payload, and js-snippets' timeline-wire.test.ts
// decodes that same payload. A change to the layout is a NEW version (new
// magic, new media type), never an edit to this one.

const WIRE_TYPE = "application/vnd.gsm.timeline.v1";
const WIRE_URL = "https://sites.pazer.build/js-snippets/branch/library/ui/timeline-wire.js";

// THIS MIRROR'S COLUMNS. The names are ours, not the library's — it decodes a
// layout and takes the vocabulary as a parameter, which is what lets one
// format serve more than one producer. Order is the wire order and must match
// the Go encoder exactly; the golden fixture is what proves it does.
const SCHEMA: WireSchema = {
    magic: "TLC1",
    deltaU: ["id"],
    deltaZ: ["start"],
    plain: ["dur", "status", "attempt"],
    bits: ["final"],
    strings: ["kind", "lane", "disposition", "event_type", "action", "delivery_id",
        "repo", "method", "route", "actor", "actor_name", "detail", "target"],
};

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

// The library module, loaded once at boot alongside the component. A dynamic
// import (not a static URL import) for the same two reasons the component
// uses one: a failed fetch must degrade to "chart loading…" rather than kill
// this module, and node — which runs the harnesses against the built file —
// cannot resolve an https specifier at load time.
type WireModule = typeof import(
    'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-wire.js');
let wire: WireModule | null = null;

/** The loaded library, or a clear failure. Everything below runs after boot. */
function w(): WireModule {
    if (!wire) throw new Error("timeline: wire module not loaded");
    return wire;
}

/** Rows per generator step here — the library's own STEP governs its decode;
 *  this is for the interval building that stays on this side. */
const STEP = 1024;

/** The ceiling a chunk is sized against, and what recordSlice warns on. */
const FRAME_BUDGET_MS = 8;

// Handing intervals to the component is main-thread work too — it ingests,
// re-packs sub-tracks and marks the canvas dirty — so a feed is one chunk,
// one frame. Unlike a decode step it CANNOT be accumulated to fill a chunk:
// each call makes the component redraw, so several in a frame would draw the
// whole grown chart at once. That is why a feed is the one thing still sized
// adaptively — its cost per interval is not fixed.
//
// The CAP matters as much as the target, because the cost is not linear in the
// feed alone: a merge gets dearer as the chart holds more, so an estimate from
// an early cheap call overshoots later ones (measured: the size ran up to 2048
// and those calls then cost 5.5-6.0 ms). 512 is the largest size measured to
// stay comfortably inside the budget once the chart is full, and the adaptive
// size still shrinks below it on a slower machine. The first call is smaller
// again: it is the one that initializes the canvas.
const TARGET_FEED_MS = 4;
const MAX_FEED_INTERVALS = 512;
const START_FEED_INTERVALS = 128;

// Steer the feed size toward TARGET_FEED_MS from what the last call cost,
// capped per step so one cheap call cannot license a huge one.
function nextFeedSize(size: number, ms: number): number {
    const ceiling = Math.min(size * 4, MAX_FEED_INTERVALS);
    if (ms <= 0) return ceiling;
    return Math.max(32, Math.min(ceiling, Math.round((size * TARGET_FEED_MS) / ms) || 32));
}

/** Walks [0,n) in STEP-sized ranges, yielding after each. */
function* chunked(n: number, work: (from: number, to: number) => void): Task<void> {
    for (let i = 0; i < n; ) {
        const to = Math.min(i + STEP, n);
        work(i, to);
        i = to;
        yield;
    }
}

// The DEMO PREVIEW's decoder — not a wire fallback. /api/timeline speaks only
// the columnar format; this exists because the backend-free styling preview
// replaces the element's fetcher with one that hands over a canned
// TimelineResponse literal from assets/demo-data.js (a hand-maintained JS
// file, which binary cannot be). It lands in the SAME column representation,
// so everything downstream — intervals, lanes, tooltips — has one shape to
// handle, and it is sliced too: JSON.parse is one unbreakable native call, but
// the transpose after it is ours. shipbench.mjs also drives it, to keep the
// measurement that chose columnar reproducible.
export function* pageFromJSONGen(resp: TimelineResponse): Task<DecodedPage> {
    const events = resp.events ?? [];
    const n = events.length;
    const c: Columns = {
        n,
        u: { id: new Float64Array(n) },
        z: { start: new Float64Array(n) },
        p: { dur: new Int32Array(n), status: new Int32Array(n), attempt: new Int32Array(n) },
        b: { final: new Uint8Array((n + 7) >> 3) },
        s: {} as Record<string, StringColumn>,
    };
    const dicts = new Map<string, { dict: string[]; seen: Map<string, number>; idx: Int32Array }>();
    for (const name of SCHEMA.strings) {
        dicts.set(name, { dict: [""], seen: new Map([["", 0]]), idx: new Int32Array(n) });
    }
    yield* chunked(n, (from, to) => {
        for (let i = from; i < to; i++) {
            const e = events[i];
            c.u.id[i] = e.id;
            c.z.start[i] = Date.parse(e.start);
            c.p.dur[i] = e.dur_ms;
            c.p.status[i] = e.status ?? 0;
            c.p.attempt[i] = e.attempt ?? 0;
            if (e.final) c.b.final[i >> 3] |= 1 << (i & 7);
            for (const name of SCHEMA.strings) {
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
    for (const name of SCHEMA.strings) {
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
    return wholePage(w().drain(w().decodePageGen(buf, SCHEMA)));
}

export function pageFromJSON(resp: TimelineResponse): TimelinePage {
    return wholePage(w().drain(pageFromJSONGen(resp)));
}

function wholePage(d: DecodedPage): TimelinePage {
    const batches: IntervalBatch[] = [];
    w().drain(intervalsGen(d.c, (b) => batches.push(b)));
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
    const startCol = c.z.start, durCol = c.p.dur, statusCol = c.p.status, idCol = c.u.id;

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

    for (let from = 0; from < c.n; ) {
        const to = Math.min(from + STEP, c.n);
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
    return w().rowOfId(c, "id", id);
}

export function eventAt(c: Columns, i: number): TimelineEvent {
    return {
        id: c.u.id[i],
        kind: str(c.s.kind, i),
        lane: str(c.s.lane, i),
        start: new Date(c.z.start[i]).toISOString(),
        dur_ms: c.p.dur[i],
        disposition: str(c.s.disposition, i),
        event_type: str(c.s.event_type, i),
        action: str(c.s.action, i),
        delivery_id: str(c.s.delivery_id, i),
        repo: str(c.s.repo, i),
        method: str(c.s.method, i),
        route: str(c.s.route, i),
        status: c.p.status[i],
        actor: str(c.s.actor, i),
        actor_name: str(c.s.actor_name, i),
        detail: str(c.s.detail, i),
        target: str(c.s.target, i),
        attempt: c.p.attempt[i],
        final: w().bitAt(c, "final", i),
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
        if (!(await this.loadLibraryForever())) return; // disconnected while retrying
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
        await w().nextFrame();
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
    // Both library modules — the component and the wire codec — on one
    // forever-retrying loop. Neither is vendored and neither may kill this
    // module if the site is briefly unreachable: the element just keeps saying
    // "chart loading…". The cache-buster is required because browsers memoize
    // a FAILED module fetch, so a bare retry of the same URL never re-requests.
    private async loadLibraryForever(): Promise<boolean> {
        for (let attempt = 0; ; attempt++) {
            if (!this.isConnected) return false;
            const bust = (url: string): string => (attempt === 0 ? url : `${url}?retry=${attempt}`);
            try {
                const [, mod] = await Promise.all([
                    import(bust(COMPONENT_URL)),
                    import(bust(WIRE_URL)) as Promise<WireModule>,
                ]);
                wire = mod;
                return true;
            } catch (e) {
                console.error(`timeline: library load failed (retry in ${COMPONENT_RETRY_MS}ms):`, e);
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
                ? await w().runSliced(pageFromJSONGen(await fetcher(path)), this.recordSlice)
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

        // ONE unit of work per frame — either build the next chunk of
        // intervals, or hand one feed to the component (which redraws off it,
        // so a feed owns its frame). Ready intervals are fed before more are
        // built, so the chart paints early and the queue never runs ahead of
        // what has been handed over.
        const ready: TimelineInterval[] = [];
        const task = intervalsGen(page.c, (b) => {
            for (const { lane, kind } of b.lanes) {
                if (!this.laneKinds.has(lane)) this.laneKinds.set(lane, kind);
            }
            for (const iv of b.intervals) ready.push(iv);
        });
        let empty = true;
        let building = true;
        while (building || ready.length) {
            await w().nextFrame(); // claim the frame first — see runSliced
            if (!this.isConnected || this.view !== tl) return; // tab left mid-merge
            if (ready.length) {
                const part = ready.splice(0, this.feedSize);
                empty = false;
                const t0 = performance.now();
                this.feed(tl, part, coverage);
                const ms = performance.now() - t0;
                this.feedSize = nextFeedSize(this.feedSize, ms);
                this.recordSlice(ms);
            } else {
                // A chunk here is the same thing runSliced makes it: keep
                // building until a frame's worth of work is done.
                const t0 = performance.now();
                do {
                    building = !task.next().done;
                } while (building && performance.now() - t0 < w().CHUNK_MS);
                this.recordSlice(performance.now() - t0);
            }
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
