// The Requests tab's live log: <gsm-request-log>.

import type { RequestEvent } from "./types";

/** How many events the browser keeps. The oldest leave first. */
export const LOG_CAP = 20000;
/** Rows drawn before "Show more". The DOM holds the window and no more. */
const PAGE = 100;
/** The least time between renders. */
const MIN_RENDER_GAP_MS = 100;
/** Occurrence times kept per group for its detail panel. */
const GROUP_TIMES = 10;

export type LogSort = "first" | "last" | "count";

export interface LogGroup {
    key: string;
    sample: RequestEvent;
    count: number;
    firstSeq: number;
    lastSeq: number;
    firstAt: string;
    lastAt: string;
    /** The newest occurrence times, oldest first. */
    times: string[];
    hay: string;
    /** Bumps on every change, so a row redraws only when its group moved. */
    version: number;
    dead: boolean;
}

export interface LogQuery {
    grouped: boolean;
    sort: LogSort;
    filter: string;
    /** Dispositions to hide. */
    hidden: ReadonlySet<string>;
    limit: number;
}

export interface LogResult {
    groups: LogGroup[];
    events: RequestEvent[];
    /** Rows that pass the filter, before the limit. */
    matched: number;
}

function groupKey(e: RequestEvent): string {
    return [e.method, e.path, e.actor || "", e.disposition, String(e.status || 0), e.reason || ""].join("\u0000");
}

function hayOf(e: RequestEvent): string {
    return [e.method, e.path, e.actor_name || "", e.actor || "", e.disposition, e.status ? String(e.status) : "", e.reason || ""]
        .join(" ").toLowerCase();
}

/** The buffer and its groups. An insert costs time in proportion to the batch, not to the buffer. */
export class RequestLogModel {
    private events: RequestEvent[] = [];
    private head = 0;
    private groups = new Map<string, LogGroup>();
    /** Groups in first-seen order, so the default view needs no sort. */
    private order: LogGroup[] = [];
    private deadCount = 0;
    /** Events at or below this Seq are ignored: the user cleared them. */
    private floor = 0;
    private hays = new WeakMap<RequestEvent, string>();
    private readonly cap: number;
    maxSeq = 0;

    constructor(cap = LOG_CAP) { this.cap = cap; }

    get size(): number { return this.events.length - this.head; }
    get groupCount(): number { return this.groups.size; }
    private get minSeq(): number { return this.size ? this.events[this.head]!.seq : 0; }

    group(key: string): LogGroup | undefined { return this.groups.get(key); }

    /** Adds events in any order. Returns how many were new. */
    ingest(batch: ReadonlyArray<RequestEvent>): number {
        const sorted = batch.filter((e) => e.seq > this.floor).sort((a, b) => a.seq - b.seq);
        const min = this.minSeq;
        const older: RequestEvent[] = [];
        let added = 0;
        let last = this.maxSeq;
        for (const e of sorted) {
            if (e.seq > last) {
                this.events.push(e);
                this.addToGroup(e);
                last = e.seq;
                added++;
            } else if (min > 0 && e.seq < min && (older.length === 0 || e.seq > older[older.length - 1]!.seq)) {
                older.push(e);
            }
        }
        this.maxSeq = last;
        if (older.length) {
            // History arrived after the live events: put it in front and count again.
            this.events = older.concat(this.events.slice(this.head));
            this.head = 0;
            added += older.length;
            this.trim();
            this.rebuild();
        } else {
            this.trim();
        }
        return added;
    }

    /** Drops everything held, and everything older that arrives later. */
    clear(): void {
        this.floor = this.maxSeq;
        this.drop();
    }

    /** Forgets everything, the cursor included. Use it when the server restarted. */
    reset(): void {
        this.floor = 0;
        this.maxSeq = 0;
        this.drop();
    }

    private drop(): void {
        this.events = [];
        this.head = 0;
        this.groups.clear();
        this.order = [];
        this.deadCount = 0;
    }

    private trim(): void {
        while (this.size > this.cap) {
            const e = this.events[this.head++]!;
            const g = this.groups.get(groupKey(e));
            if (!g) continue;
            g.count--;
            g.version++;
            if (g.count <= 0) {
                g.dead = true;
                this.deadCount++;
                this.groups.delete(g.key);
            }
        }
        if (this.head > 4096 && this.head > this.events.length / 2) {
            this.events = this.events.slice(this.head);
            this.head = 0;
        }
        if (this.deadCount > 256 && this.deadCount > this.order.length / 2) {
            this.order = this.order.filter((g) => !g.dead);
            this.deadCount = 0;
        }
    }

    private rebuild(): void {
        this.groups.clear();
        this.order = [];
        this.deadCount = 0;
        for (let i = this.head; i < this.events.length; i++) this.addToGroup(this.events[i]!);
    }

    private addToGroup(e: RequestEvent): void {
        const key = groupKey(e);
        let g = this.groups.get(key);
        if (!g) {
            g = {
                key, sample: e, count: 0, firstSeq: e.seq, lastSeq: e.seq, firstAt: e.at, lastAt: e.at,
                times: [], hay: hayOf(e), version: 0, dead: false,
            };
            this.groups.set(key, g);
            this.order.push(g);
        }
        g.count++;
        g.lastSeq = e.seq;
        g.lastAt = e.at;
        g.sample = e;
        g.times.push(e.at);
        if (g.times.length > GROUP_TIMES) g.times.shift();
        g.version++;
    }

    private hay(e: RequestEvent): string {
        let text = this.hays.get(e);
        if (text === undefined) {
            text = hayOf(e);
            this.hays.set(e, text);
        }
        return text;
    }

    query(q: LogQuery): LogResult {
        const terms = q.filter.toLowerCase().split(/\s+/).filter(Boolean);
        const pass = (disp: string, hay: () => string): boolean => {
            if (q.hidden.has(disp)) return false;
            if (terms.length === 0) return true;
            const text = hay();
            return terms.every((t) => text.includes(t));
        };
        const out: LogResult = { groups: [], events: [], matched: 0 };
        if (!q.grouped) {
            for (let i = this.events.length - 1; i >= this.head; i--) {
                const e = this.events[i]!;
                if (!pass(e.disposition, () => this.hay(e))) continue;
                if (out.matched++ < q.limit) out.events.push(e);
            }
            return out;
        }
        if (q.sort === "first") {
            for (let i = this.order.length - 1; i >= 0; i--) {
                const g = this.order[i]!;
                if (g.dead || !pass(g.sample.disposition, () => g.hay)) continue;
                if (out.matched++ < q.limit) out.groups.push(g);
            }
            return out;
        }
        const all: LogGroup[] = [];
        for (const g of this.groups.values()) {
            if (pass(g.sample.disposition, () => g.hay)) all.push(g);
        }
        all.sort(q.sort === "count"
            ? (a, b) => b.count - a.count || b.lastSeq - a.lastSeq
            : (a, b) => b.lastSeq - a.lastSeq);
        out.matched = all.length;
        out.groups = all.slice(0, q.limit);
        return out;
    }
}

// ---- the element ----

const DISP_CLASS: Readonly<Record<string, string>> = {
    hit: "applied", miss: "invalidated", passthrough: "ignored", write: "write", error: "error",
};
const DISPOSITIONS: ReadonlyArray<string> = ["hit", "miss", "passthrough", "write", "error"];
const COLUMNS: ReadonlyArray<string> = ["Result", "Upstream", "Method", "Caller", "Count", "Last seen", "Path"];
const CELL_CLASS: ReadonlyArray<string> = ["", "", "wh-event", "wh-caller", "num", "wh-when", "wh-path"];

function h(tag: string, className?: string, text?: string): HTMLElement {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
}

function clock(at: string): string {
    const d = new Date(at);
    return isNaN(d.getTime()) ? at : d.toLocaleTimeString([], { hour12: false });
}

function stamp(at: string): string {
    const d = new Date(at);
    if (isNaN(d.getTime())) return at;
    return d.toLocaleString() + " +" + String(d.getMilliseconds()).padStart(3, "0") + "ms";
}

interface RowView {
    tr: HTMLTableRowElement;
    detail: HTMLTableRowElement | null;
    cells: HTMLElement[];
    version: number;
}

interface Item {
    key: string;
    event: RequestEvent;
    group: LogGroup | null;
}

if (typeof customElements !== "undefined") {
    class GsmRequestLog extends HTMLElement {
        readonly model = new RequestLogModel();
        /** Turns a passthrough reason code into its sentence. app.ts sets it. */
        describeReason: (reason: string) => string = (r) => r;
        /** The longest render so far, for the frame-budget check. */
        worstRenderMs = 0;

        private built = false;
        private grouped = true;
        private sort: LogSort = "first";
        private filter = "";
<<<<<<< HEAD
        private hiddenDisps = new Set<string>();
=======
        /** Dispositions the user turned off. Not HTMLElement.hidden, hence the name. */
        private hiddenDisp = new Set<string>();
>>>>>>> 37e6ac2786d51da12795cf8c60d1427a7d7d62a2
        private limit = PAGE;
        private paused = false;
        private heldWhilePaused = 0;
        private dirty = true;
        private renderTimer: ReturnType<typeof setTimeout> | null = null;
        private nextAllowed = 0;
        private open = new Set<string>();
        private views = new Map<string, RowView>();

        private tbody!: HTMLElement;
        private status!: HTMLElement;
        private foot!: HTMLElement;
        private pauseBtn!: HTMLElement;
        private sortSel!: HTMLSelectElement;
        private readonly onVisible = (): void => { if (!document.hidden) this.schedule(); };

        ingest(events: ReadonlyArray<RequestEvent>): void {
            const added = this.model.ingest(events);
            if (!added) return;
            if (this.paused) this.heldWhilePaused += added;
            this.invalidate();
        }

        /** Forgets the whole log. Use it when the server restarted. */
        reset(): void {
            this.model.reset();
            this.open.clear();
            this.heldWhilePaused = 0;
            this.invalidate();
        }

        connectedCallback(): void {
            if (!this.built) this.build();
            document.addEventListener("visibilitychange", this.onVisible);
            this.schedule();
        }

        disconnectedCallback(): void {
            document.removeEventListener("visibilitychange", this.onVisible);
            if (this.renderTimer !== null) clearTimeout(this.renderTimer);
            this.renderTimer = null;
        }

        private build(): void {
            this.built = true;
            const bar = h("div", "reqlog-bar");

            const search = h("input", "reqlog-filter") as HTMLInputElement;
            search.type = "search";
            search.placeholder = "Filter by path, caller, method, status, reason";
            search.setAttribute("aria-label", "Filter the request log");
            search.addEventListener("input", () => { this.filter = search.value; this.limit = PAGE; this.invalidate(); });
            bar.appendChild(search);

            const chips = h("span", "reqlog-chips");
            for (const d of DISPOSITIONS) {
                const chip = h("button", "disp " + DISP_CLASS[d], d);
                chip.title = "Show or hide " + d + " requests";
                chip.setAttribute("aria-pressed", "true");
                chip.addEventListener("click", () => {
<<<<<<< HEAD
                    const off = !this.hiddenDisps.has(d);
                    if (off) this.hiddenDisps.add(d);
                    else this.hiddenDisps.delete(d);
=======
                    const off = !this.hiddenDisp.has(d);
                    if (off) this.hiddenDisp.add(d);
                    else this.hiddenDisp.delete(d);
>>>>>>> 37e6ac2786d51da12795cf8c60d1427a7d7d62a2
                    chip.classList.toggle("off", off);
                    chip.setAttribute("aria-pressed", off ? "false" : "true");
                    this.limit = PAGE;
                    this.invalidate();
                });
                chips.appendChild(chip);
            }
            bar.appendChild(chips);

            const groupBox = h("input") as HTMLInputElement;
            groupBox.type = "checkbox";
            groupBox.checked = true;
            groupBox.addEventListener("change", () => {
                this.grouped = groupBox.checked;
                this.sortSel.disabled = !this.grouped;
                this.limit = PAGE;
                this.invalidate();
            });
            const groupLabel = h("label", "reqlog-check");
            groupLabel.append(groupBox, " Group identical requests");
            bar.appendChild(groupLabel);

            this.sortSel = h("select", "reqlog-sort") as HTMLSelectElement;
            this.sortSel.setAttribute("aria-label", "Sort the groups");
            for (const [value, label] of [["first", "Newest kind first"], ["last", "Most recent first"], ["count", "Most frequent first"]] as const) {
                const opt = h("option", undefined, label) as HTMLOptionElement;
                opt.value = value;
                this.sortSel.appendChild(opt);
            }
            this.sortSel.addEventListener("change", () => { this.sort = this.sortSel.value as LogSort; this.invalidate(); });
            bar.appendChild(this.sortSel);

            this.pauseBtn = h("button", "btn btn-sm reqlog-pause", "Pause");
            this.pauseBtn.addEventListener("click", () => {
                this.paused = !this.paused;
                this.heldWhilePaused = 0;
                this.invalidate();
            });
            bar.appendChild(this.pauseBtn);

            const clearBtn = h("button", "btn btn-sm btn-ghost", "Clear");
            clearBtn.title = "Empty the log. New requests still arrive.";
            clearBtn.addEventListener("click", () => {
                this.model.clear();
                this.open.clear();
                this.heldWhilePaused = 0;
                this.invalidate();
            });
            bar.appendChild(clearBtn);

            this.status = h("span", "reqlog-status");
            bar.appendChild(this.status);
            this.appendChild(bar);

            const table = h("table", "webhooks rows-collapsible cols-reqlog");
            const headRow = h("tr");
            for (const c of COLUMNS) headRow.appendChild(h("th", undefined, c));
            const thead = h("thead");
            thead.appendChild(headRow);
            this.tbody = h("tbody");
            table.append(thead, this.tbody);
            const wrap = h("div", "table-wide");
            wrap.appendChild(table);
            this.appendChild(wrap);

            this.foot = h("div", "reqlog-foot");
            this.appendChild(this.foot);
        }

        private invalidate(): void {
            this.dirty = true;
            this.schedule();
        }

        // One render per change at most. A slow render pushes the next one back, so drawing never takes most of the main thread.
        private schedule(): void {
            if (!this.dirty || !this.built || !this.isConnected || document.hidden || this.renderTimer !== null) return;
            this.renderTimer = setTimeout(() => {
                requestAnimationFrame(() => {
                    this.renderTimer = null;
                    if (!this.dirty || !this.isConnected || document.hidden) return;
                    this.dirty = false;
                    const t0 = performance.now();
                    this.render();
                    const cost = performance.now() - t0;
                    if (cost > this.worstRenderMs) this.worstRenderMs = cost;
                    this.nextAllowed = performance.now() + Math.max(MIN_RENDER_GAP_MS, cost * 4);
                    this.schedule();
                });
            }, Math.max(0, this.nextAllowed - performance.now()));
        }

        private render(): void {
            this.pauseBtn.textContent = this.paused
                ? "Resume" + (this.heldWhilePaused ? " (+" + this.heldWhilePaused.toLocaleString() + ")" : "")
                : "Pause";
            this.pauseBtn.classList.toggle("btn-primary", this.paused);
            const held = this.model.size.toLocaleString() + " request" + (this.model.size === 1 ? "" : "s") + " held";
            const kinds = this.model.groupCount;
            this.status.textContent = (this.grouped ? kinds.toLocaleString() + " group" + (kinds === 1 ? "" : "s") + " · " : "") + held;
            // A paused log keeps what is on screen. Only the counters above move.
            if (this.paused) return;

<<<<<<< HEAD
            const res = this.model.query({ grouped: this.grouped, sort: this.sort, filter: this.filter, hidden: this.hiddenDisps, limit: this.limit });
=======
            const res = this.model.query({ grouped: this.grouped, sort: this.sort, filter: this.filter, hidden: this.hiddenDisp, limit: this.limit });
>>>>>>> 37e6ac2786d51da12795cf8c60d1427a7d7d62a2
            const items: Item[] = this.grouped
                ? res.groups.map((g) => ({ key: "g" + g.key, event: g.sample, group: g }))
                : res.events.map((e) => ({ key: "e" + e.seq, event: e, group: null }));

            const seen = new Set<string>();
            let cursor: ChildNode | null = this.tbody.firstChild;
            const place = (node: ChildNode): void => {
                if (cursor === node) cursor = cursor.nextSibling;
                else this.tbody.insertBefore(node, cursor);
            };
            for (const item of items) {
                let view = this.views.get(item.key);
                if (!view) {
                    view = this.createView(item);
                    this.views.set(item.key, view);
                } else if (view.version !== (item.group?.version ?? 0)) {
                    this.fill(view, item);
                }
                seen.add(item.key);
                place(view.tr);
                if (view.detail) place(view.detail);
            }
            while (cursor) {
                const next: ChildNode | null = cursor.nextSibling;
                cursor.remove();
                cursor = next;
            }
            for (const key of this.views.keys()) if (!seen.has(key)) this.views.delete(key);

            this.foot.textContent = "";
            if (res.matched === 0) {
                this.foot.textContent = this.model.size === 0
                    ? "No requests yet. They appear here as clients call the mirror."
                    : "Nothing matches the filter.";
                return;
            }
            const what = this.grouped ? "groups" : "requests";
            this.foot.appendChild(h("span", undefined, "Showing " + items.length.toLocaleString() + " of " + res.matched.toLocaleString() + " " + what));
            if (res.matched > items.length) {
                const more = h("button", "btn btn-sm", "Show more");
                more.addEventListener("click", () => { this.limit += PAGE; this.invalidate(); });
                this.foot.appendChild(more);
            }
        }

        private createView(item: Item): RowView {
            const tr = h("tr", "row-toggle") as HTMLTableRowElement;
            tr.tabIndex = 0;
            tr.setAttribute("role", "button");
            const cells = CELL_CLASS.map((cls) => tr.appendChild(h("td", cls || undefined)));
            const view: RowView = { tr, detail: null, cells, version: -1 };
            const key = item.key;
            const toggle = (): void => {
                if (this.open.has(key)) this.open.delete(key);
                else this.open.add(key);
                // A group object can be replaced while its row lives, so read it again.
                const group = item.group ? this.model.group(item.group.key) ?? item.group : null;
                this.applyOpen(view, { key, event: group ? group.sample : item.event, group });
            };
            tr.addEventListener("click", toggle);
            tr.addEventListener("keydown", (ev) => {
                if (ev.key === "Enter" || ev.key === " ") {
                    ev.preventDefault();
                    toggle();
                }
            });
            this.fill(view, item);
            this.applyOpen(view, item);
            return view;
        }

        private applyOpen(view: RowView, item: Item): void {
            const open = this.open.has(item.key);
            view.tr.classList.toggle("open", open);
            view.tr.setAttribute("aria-expanded", open ? "true" : "false");
            if (!open) {
                view.detail?.remove();
                view.detail = null;
                return;
            }
            if (!view.detail) {
                view.detail = h("tr", "row-detail open") as HTMLTableRowElement;
                const td = h("td") as HTMLTableCellElement;
                td.colSpan = COLUMNS.length;
                view.detail.appendChild(td);
                view.tr.after(view.detail);
            }
            this.fillDetail(view, item);
        }

        private fill(view: RowView, item: Item): void {
            const e = item.event;
            const cell = (i: number): HTMLElement => view.cells[i]!;
            if (view.version === -1) {
                // These cells never change for one key: the key is made of them.
                const chip = h("span", "disp " + (DISP_CLASS[e.disposition] ?? "error"), e.disposition);
                if (e.reason) chip.title = this.describeReason(e.reason);
                cell(0).appendChild(chip);
                if (e.status) {
                    const cls = e.status >= 500 ? "err" : e.status >= 400 ? "warn" : e.status >= 300 ? "redir" : "ok";
                    cell(1).appendChild(h("span", "status-code " + cls, String(e.status)));
                } else {
                    cell(1).textContent = "—";
                }
                cell(2).textContent = e.method;
                cell(3).textContent = e.actor_name || e.actor || "";
                cell(6).textContent = e.path;
            }
            cell(4).textContent = item.group ? "×" + item.group.count.toLocaleString() : "";
            cell(5).textContent = clock(item.group ? item.group.lastAt : e.at);
            view.version = item.group?.version ?? 0;
            if (view.detail) this.fillDetail(view, item);
        }

        private fillDetail(view: RowView, item: Item): void {
            const e = item.event;
            const g = item.group;
            const who = e.actor_name ? e.actor_name + "  (" + e.actor + ")" : e.actor;
            const pairs: Array<[string, string]> = [
                ["Path", e.path],
                ["Request", e.method + " → " + e.disposition + (e.status ? " (upstream " + e.status + ")" : " (no upstream call)")],
                ["Why", e.reason ? this.describeReason(e.reason) : ""],
                ["Caller", who],
            ];
            if (g) {
                pairs.push(
                    ["Seen", g.count.toLocaleString() + " time" + (g.count === 1 ? "" : "s") + " in the held log"],
                    ["First", stamp(g.firstAt)],
                    ["Last", stamp(g.lastAt)],
                    ["Latest", [...g.times].reverse().map(clock).join("  ")],
                );
            } else {
                pairs.push(["When", stamp(e.at)]);
            }
            const dl = h("dl", "row-detail-grid");
            for (const [label, value] of pairs) {
                if (!value) continue;
                dl.append(h("dt", undefined, label), h("dd", undefined, value));
            }
            view.detail!.firstElementChild!.replaceChildren(dl);
        }
    }

    customElements.define("gsm-request-log", GsmRequestLog);
}

/** The element's public face, for app.ts. */
export interface GsmRequestLogElement extends HTMLElement {
    readonly model: RequestLogModel;
    describeReason: (reason: string) => string;
    worstRenderMs: number;
    ingest(events: ReadonlyArray<RequestEvent>): void;
    reset(): void;
}
