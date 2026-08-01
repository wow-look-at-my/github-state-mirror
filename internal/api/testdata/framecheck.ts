// Drives the SHIPPED sliced decode (internal/api/web/assets/timeline.js) over a
// real payload and reports what one frame's worth of loading costs.
//
// The requirement it exists to check: the loader runs ONE chunk per frame, and
// a chunk costs less than a frame's budget — so the average main-thread work
// per frame across the whole load stays under FRAME_BUDGET_MS (8), and no
// single chunk blows past it either.
//
//   npm run build
//   GSM_DUMP=/tmp go test ./internal/api -run TestTimelineWireDumpPayloads -v
//   node internal/api/testdata/framecheck.ts /tmp/timeline-1h.bin [/tmp/timeline.json]
//
// TestTimelineFrameBudget runs exactly this and asserts the numbers.
import { readFileSync } from "node:fs";
// TYPES from the source, MODULE from the build. That split is the whole reason
// these harnesses are TypeScript: they must exercise the exact bytes the server
// embeds, but they should fail to COMPILE — not at runtime, halfway through a
// measurement — when the API they drive changes shape. It has happened:
// runSliced lost a parameter, this harness kept passing the old one, and it
// surfaced as "pacer.charge is not a function" mid-run.
import type * as Timeline from "../web/src/timeline.ts";
import type { TimelineResponse } from "../web/src/types.d.ts";

// The module registers a custom element at import time; stub the two DOM
// globals that costs so the shipped file needs no test-only accommodation.
const g = globalThis as unknown as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define(): void {} };

const mod = (await import(process.env.GSM_TIMELINE_JS ??
    new URL("../web/assets/timeline.js", import.meta.url).href)) as typeof Timeline;

const bin = new Uint8Array(readFileSync(process.argv[2] ?? "timeline.bin"));
const jsonText = process.argv[3] ? readFileSync(process.argv[3], "utf8") : null;

// Named off the module's own signatures rather than restated here — restating
// them is how a harness drifts from what it drives.
type Decoded = Awaited<ReturnType<typeof Timeline.fetchDecoded>>;
type Chunks = ReturnType<typeof Timeline.pageColumnsGen>;

async function measure(name: string, makeTask: () => Chunks): Promise<{ worst: number; avg: number }> {
    let worst = 0, chunks = 0, total = 0;
    // One chunk per frame, so a chunk's cost IS a frame's load.
    const onSlice = (ms: number): void => {
        chunks++;
        total += ms;
        if (ms > worst) worst = ms;
    };
    const t0 = performance.now();
    const decoded: Decoded = await mod.runSliced(makeTask(), onSlice);
    // Interval building is driven by the element the same way.
    const batches: Array<{ intervals: unknown[] }> = [];
    await mod.runSliced(mod.intervalsGen(decoded.c, (b) => batches.push(b)), onSlice);
    const wall = performance.now() - t0;
    const n = batches.reduce((a, b) => a + b.intervals.length, 0);
    const avg = chunks ? total / chunks : 0;
    console.log(`${name}: avg/frame ${avg.toFixed(2)}ms | worst chunk ${worst.toFixed(2)}ms | frames ${chunks} | cpu ${total.toFixed(1)}ms | wall ${wall.toFixed(1)}ms | ${n} intervals`);
    return { worst, avg };
}

const a = await measure("columnar (sliced)", () => mod.pageColumnsGen(bin));
let b = { worst: 0, avg: 0 };
if (jsonText !== null) {
    const parsed = JSON.parse(jsonText) as TimelineResponse;
    b = await measure("json fallback (sliced)", () => mod.pageFromJSONGen(parsed));
}

// Straight-line, for comparison: what the element would block for if it ran
// the whole decode in one task.
const t0 = performance.now();
mod.pageFromWire(bin);
console.log(`\nunsliced pageFromWire (one block): ${(performance.now() - t0).toFixed(1)}ms`);
console.log(`WORST_SLICE_MS ${Math.max(a.worst, b.worst).toFixed(3)}`);
console.log(`AVG_FRAME_MS ${Math.max(a.avg, b.avg).toFixed(3)}`);
