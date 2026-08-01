// Drives the SHIPPED sliced decode (internal/api/web/assets/timeline.js) over a
// real payload and reports the longest synchronous stretch of main-thread work.
//
// The requirement it exists to check: decoding must never block a frame. The
// element's own budget is SLICE_BUDGET_MS and the true bound is that plus one
// adaptive chunk; anything beyond FRAME_BUDGET_MS (8) is a dropped frame.
//
//   npm run build
//   GSM_DUMP=/tmp go test ./internal/api -run TestTimelineWireDumpPayloads -v
//   node prototype/timelinewire/framecheck.mjs /tmp/timeline-1h.bin [/tmp/timeline.json]
//
// TestTimelineFrameBudget runs exactly this and asserts the number.
globalThis.HTMLElement = class {};
globalThis.customElements = { define() {} };
const mod = await import(process.env.GSM_TIMELINE_JS ?? "/home/user/github-state-mirror/internal/api/web/assets/timeline.js");
import { readFileSync } from "node:fs";

const bin = new Uint8Array(readFileSync(process.argv[2] ?? "timeline.bin"));
const jsonText = process.argv[3] ? readFileSync(process.argv[3], "utf8") : null;

async function measure(name, makeTask) {
    let worst = 0, slices = 0, total = 0;
    const onSlice = (ms) => { slices++; total += ms; if (ms > worst) worst = ms; };
    const t0 = performance.now();
    // (task, pacer, onSlice) — pass no pacer so runSliced builds its own; in
    // node every task is its own frame, so this measures per-task work.
    const decoded = await mod.runSliced(makeTask(), undefined, onSlice);
    // Interval building is driven by the element the same way.
    const batches = [];
    await mod.runSliced(mod.intervalsGen(decoded.c, (b) => batches.push(b)), undefined, onSlice);
    const wall = performance.now() - t0;
    const n = batches.reduce((a, b) => a + b.intervals.length, 0);
    console.log(`${name}: worst slice ${worst.toFixed(2)}ms | slices ${slices} | cpu ${total.toFixed(1)}ms | wall ${wall.toFixed(1)}ms | ${n} intervals`);
    return worst;
}

const a = await measure("columnar (sliced)", () => mod.pageColumnsGen(bin));
let b = 0;
if (jsonText) b = await measure("json fallback (sliced)", () => mod.pageFromJSONGen(JSON.parse(jsonText)));

// Straight-line, for comparison: what the element would block for if it ran
// the whole decode in one task.
const t0 = performance.now();
mod.pageFromWire(bin);
console.log(`\nunsliced pageFromWire (one block): ${(performance.now() - t0).toFixed(1)}ms`);
console.log(`WORST_SLICE_MS ${Math.max(a, b).toFixed(3)}`);
