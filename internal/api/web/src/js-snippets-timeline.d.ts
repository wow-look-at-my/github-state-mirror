// INTERIM type shim for the buildhost-served <timeline-view> module — the
// same stopgap webhook-runner's dashboard uses, trimmed to exactly the surface
// this repo's adapter (src/timeline.ts) consumes. Temporary until the build
// fetches js-snippets' published declarations mechanically (the library site
// already serves a .d.ts next to every .js); never grow it beyond what the
// adapter touches.
//
// The component is NOT vendored: the browser imports it at runtime from
// js-snippets' buildhost library site (live at master head; replaced the
// quota-dead GitHub Pages deploy — the org's standard js-snippets consumption
// model), and component fixes reach this dashboard on js-snippets merge with
// no mirror change. TypeScript can't fetch types from a URL, so this ambient
// declaration provides them — TYPES ONLY, no implementation.
declare module 'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js' {
    /** A swimlane: one labeled horizontal band of the timeline. */
    export interface TimelineLane {
        /** Unique lane id — intervals reference it via `laneId`. */
        id: string;
        /** Text drawn in the left gutter (ellipsized; full text via tooltip). */
        label: string;
        /** Optional grouping key — the default color category for intervals that set none. */
        group?: string;
    }

    /** One bar on a lane: [start, end] on the shared time axis. */
    export interface TimelineInterval {
        /** Unique id — mergeData dedupes on it. */
        id: string;
        laneId: string;
        start: number | Date;
        /** null/undefined = ongoing (renders to the live "now" edge). */
        end?: number | Date | null;
        label?: string;
        /** Color key: same category = same hue. Defaults to lane.group, then laneId. */
        category?: string;
        /** Style-map key: rendering treatment (e.g. 'failed', 'dim'). */
        state?: string;
        /** Opaque consumer payload — echoed back in events and tooltip callbacks. */
        data?: unknown;
    }

    /** A time range [start, end] used by coverage bookkeeping (ms since epoch). */
    export interface TimeRange {
        start: number;
        end: number;
    }

    /** The full data payload for setData / mergeData. */
    export interface TimelineData {
        lanes?: TimelineLane[];
        intervals?: TimelineInterval[];
        /** Time range the supplied intervals fully cover (for async history). */
        coverage?: TimeRange;
    }

    /**
     * Async history loader: invoked when the viewport reaches uncovered past.
     * Supply the data via mergeData() before resolving; resolve
     * `{ exhausted: true }` when nothing exists before this range.
     */
    export type LoadRangeFn = (start: number, end: number) => Promise<{ exhausted?: boolean } | void>;

    /** What the pointer is over — handed to tooltipFor and hover/click events. */
    export type TimelineHit =
        | { type: 'interval'; interval: TimelineInterval; lane: TimelineLane }
        | { type: 'lane'; lane: TimelineLane }
        | { type: string; interval?: TimelineInterval; lane?: TimelineLane };

    /** Tooltip content callback: string or Node (never injected as HTML). */
    export type TooltipFn = (hit: TimelineHit) => string | Node | null | undefined;

    /**
     * The timeline custom element. Importing this module registers it as
     * `<timeline-view>`. Data arrives via properties and methods — never
     * attributes; the only attributes are scalar toggles (`empty-text`, ...).
     */
    export class TimelineViewElement extends HTMLElement {
        /** Replace everything supplied (omitted fields keep current data). */
        setData(data: TimelineData): void;
        /** Additive: upsert by id, union coverage — the polling path. */
        mergeData(data: TimelineData): void;
        setLanes(lanes: TimelineLane[]): void;
        get tooltipFor(): TooltipFn | null;
        set tooltipFor(fn: TooltipFn | null | undefined);
        setViewport(start: number | Date, end: number | Date): void;
        /** Async history loader (see LoadRangeFn); null disables. */
        get loadRange(): LoadRangeFn | null;
        set loadRange(fn: LoadRangeFn | null | undefined);
        /** Whether the view is docked to the live "now" edge. */
        get followNow(): boolean;
        set followNow(v: boolean);
        /** Staleness marking (feature-detect with `typeof el.markFresh ===
         * 'function'`): call markFresh() whenever the feed proves alive; if
         * staleAfterMs elapses without one the chart renders as stale instead
         * of extrapolating fiction. */
        markFresh?(): void;
        staleAfterMs?: number;
    }
}

// The wire format the chart is fed with — same library, same runtime-import
// model, same reason for a hand-maintained shim. Types only, trimmed to what
// the adapter touches: the codec decodes a LAYOUT and takes this mirror's
// column names as a schema, so nothing here carries mirror vocabulary.
declare module 'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-wire.js' {
    /** How a payload's columns are named and encoded — the consumer's own. */
    export interface WireSchema {
        magic: string;
        deltaU: readonly string[];
        deltaZ: readonly string[];
        plain: readonly string[];
        bits: readonly string[];
        strings: readonly string[];
    }

    export interface StringColumn {
        dict: string[];
        /** null when the column went unused: every row reads dict[0]. */
        idx: Int32Array | null;
    }

    /** One decoded page: typed arrays and dictionaries, no per-row objects. */
    export interface Columns {
        n: number;
        u: Record<string, Float64Array>;
        z: Record<string, Float64Array>;
        p: Record<string, Int32Array>;
        b: Record<string, Uint8Array>;
        s: Record<string, StringColumn>;
    }

    export interface DecodedPage {
        c: Columns;
        maxId: number;
        retentionStart: number;
        now: number;
    }

    /** A resumable unit of work: yields periodically, returns its result. */
    export type Task<T> = Generator<undefined, T, undefined>;

    export function decodePageGen(buf: Uint8Array, schema: WireSchema): Task<DecodedPage>;
    export function decodePage(buf: Uint8Array, schema: WireSchema): DecodedPage;
    /** One chunk per frame, a chunk being CHUNK_MS of real work. */
    export function runSliced<T>(task: Task<T>, onSlice?: (ms: number) => void): Promise<T>;
    export function nextFrame(): Promise<void>;
    export function drain<T>(task: Task<T>): T;
    export function stringAt(c: Columns, name: string, i: number): string;
    export function bitAt(c: Columns, name: string, i: number): boolean;
    export function rowOfId(c: Columns, idColumn: string, id: number): number;
    export const CHUNK_MS: number;
    export const STEP: number;
}
