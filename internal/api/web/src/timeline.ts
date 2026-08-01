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

// Bounded default fetcher. app.ts overrides `fetcher` only in demo mode (the
// backend-free preview serves canned data); production uses this one — the
// AbortSignal bound is what keeps the single-flight poll guard un-wedgeable.
//
// It asks for the COLUMNAR encoding (internal/api/timelinewire.go) and falls
// back to JSON on whatever the server actually answers with, so a rollback of
// the server half — or the demo preview, which serves canned JSON — keeps
// working with no branch anywhere else in this file.
async function fetchPage(path: string): Promise<TimelinePage> {
    const res = await fetch(path, {
        headers: { Accept: `${WIRE_TYPE}, application/json;q=0.9` },
        credentials: "same-origin",
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (!res.ok) throw new Error("HTTP " + res.status);
    if ((res.headers.get("Content-Type") ?? "").startsWith(WIRE_TYPE)) {
        return pageFromWire(new Uint8Array(await res.arrayBuffer()));
    }
    return pageFromJSON((await res.json()) as TimelineResponse);
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

// A row reference: what an interval carries instead of a materialized event.
interface EventRef {
    c: Columns;
    i: number;
}

interface TimelinePage {
    intervals: TimelineInterval[];
    // (lane, kind) pairs seen on this page, for the grouped lane ordering.
    lanes: Array<{ lane: string; kind: string }>;
    maxId: number;
    retentionStart: number; // epoch ms
    now: number; // epoch ms
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

    bytes(n: number): Uint8Array {
        if (this.p + n > this.b.length) throw new Error("timeline: truncated payload");
        const out = this.b.subarray(this.p, this.p + n);
        this.p += n;
        return out;
    }

    str(): string {
        return this.dec.decode(this.bytes(this.uvarint()));
    }

    dict(): string[] {
        const n = this.uvarint();
        const out = new Array<string>(n);
        for (let i = 0; i < n; i++) out[i] = this.str();
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

export function pageFromWire(buf: Uint8Array): TimelinePage {
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
    let id = 0;
    for (let i = 0; i < n; i++) c.id[i] = id += r.uvarint();
    let ms = 0;
    for (let i = 0; i < n; i++) c.start[i] = ms += r.varint();
    for (let i = 0; i < n; i++) c.dur[i] = r.uvarint();
    for (let i = 0; i < n; i++) c.status[i] = r.uvarint();
    for (let i = 0; i < n; i++) c.attempt[i] = r.uvarint();
    c.final = r.bytes((n + 7) >> 3);

    for (const name of COLS) {
        const dict = r.dict();
        if (dict.length === 1) {
            c.s[name] = { dict, idx: null }; // column unused in this window
            continue;
        }
        const idx = new Int32Array(n);
        for (let i = 0; i < n; i++) idx[i] = r.uvarint();
        c.s[name] = { dict, idx };
    }
    if (!r.atEnd()) throw new Error("timeline: trailing bytes in payload");
    return finishPage(c, maxId, retentionStart, now);
}

// The JSON fallback lands in the SAME column representation, so everything
// downstream — intervals, lanes, tooltips — has exactly one shape to handle.
export function pageFromJSON(resp: TimelineResponse): TimelinePage {
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
    for (let i = 0; i < n; i++) {
        const e = events[i];
        c.id[i] = e.id;
        c.start[i] = Date.parse(e.start);
        c.dur[i] = e.dur_ms;
        c.status[i] = e.status ?? 0;
        c.attempt[i] = e.attempt ?? 0;
        if (e.final) c.final[i >> 3] |= 1 << (i & 7);
        for (const name of COLS) {
            const v = (e as unknown as Record<string, unknown>)[name];
            const str = typeof v === "string" ? v : "";
            const d = dicts.get(name)!;
            let ix = d.seen.get(str);
            if (ix === undefined) {
                ix = d.dict.length;
                d.dict.push(str);
                d.seen.set(str, ix);
            }
            d.idx[i] = ix;
        }
    }
    for (const name of COLS) {
        const d = dicts.get(name)!;
        c.s[name] = { dict: d.dict, idx: d.dict.length === 1 ? null : d.idx };
    }
    return finishPage(c, resp.max_id, Date.parse(resp.retention_start), Date.parse(resp.now));
}

const str = (col: StringColumn, i: number): string => (col.idx === null ? "" : col.dict[col.idx[i]]);

// ---- columns → component translation ----

function stateForRow(c: Columns, i: number): string {
    const kind = str(c.s.kind, i);
    const disp = str(c.s.disposition, i);
    if (kind === "webhook") {
        // error = dispatch failed; unverified/rejected/unparseable = a
        // delivery refused before dispatch — all unmissable.
        if (disp === "error" || disp === "unverified" ||
            disp === "rejected" || disp === "unparseable") return "failed";
        if (disp === "ignored") return "dim"; // received but not tracked
        return "";
    }
    if (kind === "notify") {
        return disp === "failed" ? "failed" : "";
    }
    // Requests/exchanges: an upstream 5xx or a failed exchange is unmissable;
    // 4xx stays neutral (a 404 passthrough is often the legitimate answer).
    if (disp === "error" || c.status[i] >= 500) return "failed";
    return "";
}

function labelForRow(c: Columns, i: number): string {
    if (str(c.s.kind, i) === "webhook") {
        const repo = str(c.s.repo, i);
        if (repo) {
            const slash = repo.indexOf("/");
            return slash >= 0 ? repo.slice(slash + 1) : repo;
        }
        return str(c.s.action, i);
    }
    return c.status[i] ? String(c.status[i]) : "";
}

// finishPage builds one interval per row — and NOTHING else per row. The
// 19-field event object a tooltip needs is built on hover (eventAt), which is
// what makes a full-window page ~5x cheaper to render than the JSON path it
// replaced: 100k intervals now, one event object when someone points at one.
function finishPage(c: Columns, maxId: number, retentionStart: number, now: number): TimelinePage {
    const intervals = new Array<TimelineInterval>(c.n);
    const lanes = new Map<string, string>();
    for (let i = 0; i < c.n; i++) {
        const lane = str(c.s.lane, i);
        if (!lanes.has(lane)) lanes.set(lane, str(c.s.kind, i));
        const start = c.start[i];
        intervals[i] = {
            id: String(c.id[i]),
            laneId: lane,
            start,
            // end = start + the REAL measured duration. Never null (every
            // recorded event finished) and never inflated: a sub-pixel span is
            // the component's native instant pip, which is exactly right for a
            // 3ms webhook dispatch.
            end: start + Math.max(0, c.dur[i]),
            label: labelForRow(c, i),
            category: lane, // stable hue per lane
            state: stateForRow(c, i),
            data: { c, i } satisfies EventRef,
        };
    }
    return {
        intervals,
        lanes: [...lanes].map(([lane, kind]) => ({ lane, kind })),
        maxId,
        retentionStart,
        now,
    };
}

// eventAt materializes one row as the flat event the tooltip reads. Called
// once per hover, never per row.
export function eventAt(ref: EventRef): TimelineEvent {
    const { c, i } = ref;
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
    const ref = hit.interval.data as EventRef | undefined;
    if (!ref?.c) return null;
    const e = eventAt(ref);
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
    private seeded = false;
    // laneId -> kind, for the grouped lane ordering (webhooks first).
    private laneKinds = new Map<string, string>();
    private laneOrderKey = "";

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
        // Feature-detected staleness marking: with a 5s poll, ~2.5 missed
        // polls means the feed is genuinely dead — say so on the chart
        // instead of extrapolating.
        if (typeof tl.markFresh === "function") tl.staleAfterMs = STALE_AFTER_MS;
        tl.setAttribute("empty-text", "no traffic recorded yet");
        this.view = tl;
        this.appendChild(tl);
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
            const path = this.maxId > 0 ? "/api/timeline?since=" + this.maxId : "/api/timeline";
            // A demo-mode override serves canned JSON; production takes the
            // negotiated (columnar) path. Both end up as a TimelinePage.
            const fetcher = this.fetcher;
            this.apply(tl, fetcher ? pageFromJSON(await fetcher(path)) : await fetchPage(path));
        } catch (e) {
            // Keep the last data; the component's staleness marking says the
            // feed is down once STALE_AFTER_MS passes without a markFresh.
            console.error("timeline: poll failed:", e);
        } finally {
            this.pollInFlight = false;
        }
    }

    private apply(tl: TimelineViewElement, page: TimelinePage): void {
        if (page.maxId > this.maxId) this.maxId = page.maxId;
        const coverage = { start: page.retentionStart, end: page.now };
        for (const { lane, kind } of page.lanes) {
            if (!this.laneKinds.has(lane)) this.laneKinds.set(lane, kind);
        }
        if (!this.seeded) {
            this.seeded = true;
            this.laneOrderKey = "";
            const lanes = this.computeLanes();
            this.laneOrderKey = lanes.map((l) => l.id).join("\n");
            tl.setData({ lanes, intervals: page.intervals, coverage });
            const now = coverage.end || Date.now();
            tl.setViewport(now - INITIAL_WINDOW_MS, now);
            tl.followNow = true;
            this.markFresh(tl);
            return;
        }
        if (page.intervals.length > 0) {
            // MERGE, never setData: a rebuild would wipe held data and reset
            // sub-track packing mid-view. Coverage rides along so the live
            // window keeps tracking now.
            tl.mergeData({ intervals: page.intervals, coverage });
            this.syncLanes(tl);
        }
        // An empty poll is still proof the feed is alive.
        this.markFresh(tl);
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
