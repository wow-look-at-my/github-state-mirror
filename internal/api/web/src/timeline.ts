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
    TimelineData,
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
async function fetchTimeline(path: string): Promise<TimelineResponse> {
    const res = await fetch(path, {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (!res.ok) throw new Error("HTTP " + res.status);
    return (await res.json()) as TimelineResponse;
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

// ---- event → component translation ----

function stateFor(e: TimelineEvent): string {
    if (e.kind === "webhook") {
        // error = dispatch failed; unverified/rejected/unparseable = a
        // delivery refused before dispatch — all unmissable.
        if (e.disposition === "error" || e.disposition === "unverified" ||
            e.disposition === "rejected" || e.disposition === "unparseable") return "failed";
        if (e.disposition === "ignored") return "dim"; // received but not tracked
        return "";
    }
    if (e.kind === "notify") {
        return e.disposition === "failed" ? "failed" : "";
    }
    // Requests/exchanges: an upstream 5xx or a failed exchange is unmissable;
    // 4xx stays neutral (a 404 passthrough is often the legitimate answer).
    if (e.disposition === "error" || (e.status !== undefined && e.status >= 500)) return "failed";
    return "";
}

function labelFor(e: TimelineEvent): string {
    if (e.kind === "webhook") {
        if (e.repo) {
            const slash = e.repo.indexOf("/");
            return slash >= 0 ? e.repo.slice(slash + 1) : e.repo;
        }
        return e.action ?? "";
    }
    if (e.kind === "notify") {
        return e.status ? String(e.status) : "";
    }
    return e.status ? String(e.status) : "";
}

function toInterval(e: TimelineEvent): TimelineInterval {
    const start = Date.parse(e.start);
    // end = start + the REAL measured duration. Never null (every recorded
    // event finished) and never inflated: a sub-pixel span is the component's
    // native instant pip, which is exactly right for a 3ms webhook dispatch.
    return {
        id: String(e.id),
        laneId: e.lane,
        start,
        end: start + Math.max(0, e.dur_ms),
        label: labelFor(e),
        category: e.lane, // stable hue per lane
        state: stateFor(e),
        data: e,
    };
}

function tooltipFor(hit: TimelineHit): Node | null {
    if (hit.type !== "interval" || !hit.interval) return null;
    const e = hit.interval.data as TimelineEvent | undefined;
    if (!e) return null;
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
            const fetcher = this.fetcher ?? fetchTimeline;
            const path = this.maxId > 0 ? "/api/timeline?since=" + this.maxId : "/api/timeline";
            this.apply(tl, await fetcher(path));
        } catch (e) {
            // Keep the last data; the component's staleness marking says the
            // feed is down once STALE_AFTER_MS passes without a markFresh.
            console.error("timeline: poll failed:", e);
        } finally {
            this.pollInFlight = false;
        }
    }

    private apply(tl: TimelineViewElement, resp: TimelineResponse): void {
        const events = resp.events ?? [];
        if (resp.max_id > this.maxId) this.maxId = resp.max_id;
        const coverage = {
            start: Date.parse(resp.retention_start),
            end: Date.parse(resp.now),
        };
        for (const e of events) {
            if (!this.laneKinds.has(e.lane)) this.laneKinds.set(e.lane, e.kind);
        }
        if (!this.seeded) {
            this.seeded = true;
            this.laneOrderKey = "";
            const lanes = this.computeLanes();
            this.laneOrderKey = lanes.map((l) => l.id).join("\n");
            const data: TimelineData = { lanes, intervals: events.map(toInterval), coverage };
            tl.setData(data);
            const now = coverage.end || Date.now();
            tl.setViewport(now - INITIAL_WINDOW_MS, now);
            tl.followNow = true;
            this.markFresh(tl);
            return;
        }
        if (events.length > 0) {
            // MERGE, never setData: a rebuild would wipe held data and reset
            // sub-track packing mid-view. Coverage rides along so the live
            // window keeps tracking now.
            tl.mergeData({ intervals: events.map(toInterval), coverage });
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
