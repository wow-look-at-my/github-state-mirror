// Measures the SHIPPED browser decoder (internal/api/web/assets/timeline.js,
// the exact module the server embeds) against the JSON path it replaced, on
// payloads written by TestTimelineWireDumpPayloads:
//
//   npm run build
//   GSM_DUMP=/tmp go test ./internal/api -run TestTimelineWireDumpPayloads -v
//   cd /tmp && node <repo>/internal/api/testdata/shipbench.ts
//
// This is the only bench left, and deliberately so: the candidate-format
// bake-off that chose columnar was a prototype, was finished, and is gone (see
// docs/timeline-wire-format.md). What ships is what is worth measuring.
import { readFileSync } from "node:fs";
import { gzipSync } from "node:zlib";
// Types from the source, module from the build — see framecheck.ts.
import type * as Timeline from "../web/src/timeline.ts";
import type { TimelineResponse } from "../web/src/types.d.ts";

// Stub the two DOM globals the module's custom-element registration needs.
const g = globalThis as unknown as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define(): void {} };

const { pageFromWire, pageFromJSON } = (await import(process.env.GSM_TIMELINE_JS ??
    new URL("../web/assets/timeline.js", import.meta.url).href)) as typeof Timeline;

const bin = new Uint8Array(readFileSync("timeline.bin"));
const jsonText = readFileSync("timeline.json", "utf8");
const parsed = JSON.parse(jsonText) as TimelineResponse;

function bench(name: string, fn: () => number): void {
    let best = Infinity;
    let out = 0;
    for (let i = 0; i < 5; i++) {
        const t0 = process.hrtime.bigint();
        out = fn();
        const ms = Number(process.hrtime.bigint() - t0) / 1e6;
        if (ms < best) best = ms;
    }
    console.log(`${name.padEnd(46)} ${best.toFixed(1).padStart(7)} ms   ${out} intervals`);
}

console.log(`columnar ${bin.length} B (gzip ${gzipSync(bin, { level: 1 }).length} B), ` +
    `json ${jsonText.length} B (gzip ${gzipSync(Buffer.from(jsonText), { level: 1 }).length} B)\n`);
bench("SHIPPED pageFromWire (columnar)", () => pageFromWire(bin).intervals.length);
bench("SHIPPED pageFromJSON (JSON fallback)", () => pageFromJSON(parsed).intervals.length);

// Sanity: the two paths must produce identical intervals. A format that
// decodes to different data is not a faster format.
const a = pageFromWire(bin).intervals;
const b = pageFromJSON(parsed).intervals;
let bad = 0;
for (let i = 0; i < a.length; i++) {
    const x = a[i], y = b[i];
    if (x.id !== y.id || x.laneId !== y.laneId || x.start !== y.start ||
        x.end !== y.end || x.label !== y.label || x.state !== y.state) bad++;
}
console.log(`\nwire-vs-JSON interval mismatches: ${bad}`);
