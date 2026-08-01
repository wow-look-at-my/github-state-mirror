// Measures the SHIPPED browser decoder (internal/api/web/assets/timeline.js,
// the exact module the server embeds) against the JSON path it replaced, on
// payloads written by TestTimelineWireDumpPayloads:
//
//   npm run build
//   GSM_DUMP=/tmp go test ./internal/api -run TestTimelineWireDumpPayloads -v
//   cd /tmp && node <repo>/internal/api/testdata/shipbench.mjs
//
// This is the only bench left, and deliberately so: the candidate-format
// bake-off that chose columnar was a prototype, was finished, and is gone (see
// docs/timeline-wire-format.md). What ships is what is worth measuring.
// Stub the two DOM globals the module's custom-element registration needs.
globalThis.HTMLElement = class {};
globalThis.customElements = { define() {} };
const { pageFromWire, pageFromJSON } = await import(process.env.GSM_TIMELINE_JS ?? new URL("../web/assets/timeline.js", import.meta.url).href);
import { readFileSync, statSync } from "node:fs";
import { gzipSync } from "node:zlib";

const bin = new Uint8Array(readFileSync("timeline.bin"));
const jsonText = readFileSync("timeline.json", "utf8");

const bench = (name, fn) => {
    let best = Infinity, out;
    for (let i = 0; i < 5; i++) {
        const t0 = process.hrtime.bigint();
        out = fn();
        const ms = Number(process.hrtime.bigint() - t0) / 1e6;
        if (ms < best) best = ms;
    }
    console.log(`${name.padEnd(46)} ${best.toFixed(1).padStart(7)} ms   ${out} intervals`);
};

console.log(`columnar ${bin.length} B (gzip ${gzipSync(bin, {level:1}).length} B), json ${jsonText.length} B (gzip ${gzipSync(Buffer.from(jsonText), {level:1}).length} B)\n`);
bench("SHIPPED pageFromWire (columnar)", () => pageFromWire(bin).intervals.length);
bench("SHIPPED pageFromJSON (JSON fallback)", () => pageFromJSON(JSON.parse(jsonText)).intervals.length);

// Sanity: the two paths must produce identical intervals.
const a = pageFromWire(bin).intervals, b = pageFromJSON(JSON.parse(jsonText)).intervals;
let bad = 0;
for (let i = 0; i < a.length; i++) {
    if (a[i].id !== b[i].id || a[i].laneId !== b[i].laneId || a[i].start !== b[i].start ||
        a[i].end !== b[i].end || a[i].label !== b[i].label || a[i].state !== b[i].state) bad++;
}
console.log(`\nwire-vs-JSON interval mismatches: ${bad}`);
