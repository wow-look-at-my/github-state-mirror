// END-TO-END frame-budget check, in a real browser.
//
// framecheck.mjs measures our decoder under node. That is the bigger half of
// the work but not all of it: the chart also hands intervals to the
// <timeline-view> component, which ingests, packs sub-tracks and RENDERS —
// all on the same main thread. A decode that never blocks a frame is worth
// nothing if the ingest right after it blocks for 40 ms.
//
// So this boots the REAL page pieces in headless Chromium — the built
// assets/timeline.js, the real <gsm-timeline> element it registers, the real
// component (served from a local copy so the run needs no network), fed by a
// local server answering /api/timeline exactly as the mirror does — and
// reports the longest LONG TASK the browser observed, which counts everything
// on the main thread whether we wrote it or not.
//
//   npm run build
//   GSM_DUMP=/tmp go test ./internal/api -run TestTimelineWireDumpPayloads -v
//   curl -fsSL https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js -o /tmp/timeline-view.js
//   node prototype/timelinewire/browsercheck.mjs /tmp/timeline-1h.bin /tmp/timeline-view.js
//
// Prints WORST_TASK_MS for the caller to assert on.
import { createServer } from "node:http";
import { existsSync, readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { chromium } from "playwright";

const payloadPath = process.argv[2] ?? "timeline-1h.bin";
const componentPath = process.argv[3] ?? "timeline-view.js";
const modulePath = process.env.GSM_TIMELINE_JS ??
    new URL("../../internal/api/web/assets/timeline.js", import.meta.url).pathname;

const payload = readFileSync(payloadPath);
const moduleJs = readFileSync(modulePath);
const componentJs = readFileSync(componentPath);

const PAGE = `<!doctype html><meta charset="utf-8"><title>frame budget</title>
<body><gsm-timeline id="tl" style="display:block;height:600px"></gsm-timeline>
<script type="module" src="/timeline.js"></script>
<script>
// Long tasks are the browser's own verdict on main-thread blocking: anything
// over 50 ms is reported by spec, so we ALSO time every animation frame gap,
// which catches the 8-50 ms stalls this budget actually cares about.
window.__worst = 0;
window.__tasks = [];
try {
    new PerformanceObserver((l) => {
        for (const e of l.getEntries()) {
            window.__tasks.push(e.duration);
            if (e.duration > window.__worst) window.__worst = e.duration;
        }
    }).observe({ entryTypes: ["longtask"] });
} catch (e) {}
// Long Animation Frames name the culprit: total duration, how much was
// script vs rendering, and WHICH script.
window.__loaf = [];
try {
    new PerformanceObserver((l) => {
        for (const e of l.getEntries()) {
            window.__loaf.push({
                dur: Math.round(e.duration),
                blocking: Math.round(e.blockingDuration ?? 0),
                renderStart: Math.round(e.renderStart - e.startTime),
                styleLayout: Math.round(e.styleAndLayoutStart ? e.startTime + e.duration - e.styleAndLayoutStart : 0),
                scripts: (e.scripts ?? []).map((sc) => ({
                    src: (sc.sourceURL || sc.invoker || "?").slice(-60),
                    invoker: sc.invokerType,
                    dur: Math.round(sc.duration),
                })).sort((a, b) => b.dur - a.dur).slice(0, 3),
            });
        }
    }).observe({ type: "long-animation-frame", buffered: true });
} catch (e) { window.__loafErr = String(e); }
// Per-frame JS: wrap rAF so every callback's synchronous duration is timed.
// This is what "blocking a frame" actually means — it counts the component's
// draw as well as ours.
window.__rafMax = 0; window.__rafs = [];
{
    const orig = window.requestAnimationFrame.bind(window);
    window.requestAnimationFrame = (cb) => orig((t) => {
        const t0 = performance.now();
        cb(t);
        const ms = performance.now() - t0;
        if (ms > 1) window.__rafs.push({ ms: +ms.toFixed(2), at: Math.round(t0) });
        if (ms > window.__rafMax) window.__rafMax = ms;
    });
}
window.__gaps = [];
let last = performance.now();
(function tick() {
    const now = performance.now();
    // A frame gap far past the display interval means something held the
    // thread; subtract a nominal 16.7 ms frame to get the block itself.
    const gap = now - last;
    if (gap > 20) window.__gaps.push({ gap, at: Math.round(now) });
    last = now;
    requestAnimationFrame(tick);
})();

// Attribution: time every call the adapter makes INTO the component, so a
// stall can be pinned on ingest rather than blamed on the decode (or the
// other way round). Patched on the prototype before any instance exists.
window.__ingest = [];
const patch = () => {
    const proto = customElements.get("timeline-view")?.prototype;
    if (!proto || proto.__patched) return false;
    proto.__patched = true;
    // Time the component's own internals too, by name — the built module keeps
    // them on the prototype. This is what turns "a 16 ms frame" into "16 ms in
    // THIS method".
    window.__inner = {};
    for (const name of ["draw", "drawIntervals", "drawLanes", "drawMinimap", "mmPaintFull",
                        "mmRepaint", "drawAxisAndGrid", "drawMarkers", "drawConnectors",
                        "drawCoverage", "updateVisibleLayout", "clusterLane", "layoutLanes",
                        "resizeBackingStore", "drawCluster", "rebuildTracks", "packLane", "assignLane", "rebuild", "computeAutoFit", "autoGutter", "clampLaneScroll", "syncChrome"]) {
        const orig = proto[name];
        if (typeof orig !== "function") continue;
        proto[name] = function (...args) {
            const t0 = performance.now();
            const out = orig.apply(this, args);
            const ms = performance.now() - t0;
            const e = (window.__inner[name] ??= { calls: 0, total: 0, max: 0 });
            e.calls++; e.total += ms; if (ms > e.max) e.max = ms;
            (window.__innerLog ??= []).push({ name, ms: +ms.toFixed(2), at: +t0.toFixed(1) });
            return out;
        };
    }
    for (const name of ["setData", "mergeData", "setLanes", "setViewport"]) {
        const orig = proto[name];
        if (typeof orig !== "function") continue;
        proto[name] = function (...args) {
            const t0 = performance.now();
            const out = orig.apply(this, args);
            const ms = performance.now() - t0;
            const n = args[0]?.intervals?.length ?? 0;
            window.__ingest.push({ name, ms, n, at: Math.round(t0) });
            return out;
        };
    }
    return true;
};
const patchTimer = setInterval(() => { if (patch()) clearInterval(patchTimer); }, 5);
</script>`;

const server = createServer((req, res) => {
    const url = new URL(req.url, "http://localhost");
    if (url.pathname === "/") {
        res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
        res.end(PAGE);
    } else if (url.pathname === "/timeline.js") {
        res.writeHead(200, { "Content-Type": "text/javascript; charset=utf-8" });
        res.end(moduleJs);
    } else if (url.pathname === "/api/timeline") {
        // The mirror's own answer shape: columnar bytes under the negotiated
        // media type. The fixture covers the window the element asks for; a
        // ?since= poll gets an empty page (see below).
        if (process.env.GSM_EMPTY === "1" || url.searchParams.has("since")) {
            res.writeHead(200, { "Content-Type": "application/vnd.gsm.timeline.v1" });
            res.end(emptyPayload());
            return;
        }
        res.writeHead(200, { "Content-Type": "application/vnd.gsm.timeline.v1" });
        res.end(payload);
    } else {
        res.writeHead(404).end();
    }
});

// A well-formed empty page: magic + max_id + retention_start + now + n=0 +
// 13 single-entry dictionaries. Built by hand so the harness needs no server.
function emptyPayload() {
    const parts = [Buffer.from("TLC1")];
    const uvarint = (v) => {
        const out = [];
        while (v >= 0x80) { out.push((v & 0x7f) | 0x80); v = Math.floor(v / 128); }
        out.push(v);
        return Buffer.from(out);
    };
    const varint = (v) => uvarint(v < 0 ? -v * 2 - 1 : v * 2);
    parts.push(uvarint(0));                    // max_id — 0 keeps the element's cursor put
    parts.push(varint(Date.now() - 86400000)); // retention_start
    parts.push(varint(Date.now()));            // now
    parts.push(uvarint(0));                    // n
    for (let i = 0; i < 13; i++) {
        parts.push(uvarint(1)); // dict of one entry...
        parts.push(uvarint(0)); // ...the empty string
    }
    return Buffer.concat(parts);
}

await new Promise((r) => server.listen(0, r));
const port = server.address().port;

// The environment ships a prebuilt Chromium that may not match the npm
// package's expected build; point at it rather than downloading another.
const executablePath = process.env.GSM_CHROMIUM ??
    "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
const browser = await chromium.launch(
    existsSync(executablePath) ? { executablePath } : {});
const page = await browser.newPage();
page.on("console", (m) => {
    if (m.type() === "error" || m.text().includes("timeline:")) console.log("  [page]", m.text());
});
// The component is imported from its published URL — and it is CODE-SPLIT, so
// serving one file for every request under that origin deadlocks the module
// graph on a fake cycle. Serve the entry from the local copy and fetch each
// sibling chunk once through curl (the browser has no route to the internet
// here, node does).
const chunkCache = new Map();
await page.route("https://sites.pazer.build/**", (route) => {
    const url = route.request().url();
    if (url.endsWith("/ui/timeline-view.js")) {
        return route.fulfill({ status: 200, contentType: "text/javascript; charset=utf-8", body: componentJs });
    }
    if (!chunkCache.has(url)) {
        try {
            chunkCache.set(url, execFileSync("curl", ["-fsSL", url], { maxBuffer: 32 << 20 }));
        } catch {
            return route.fulfill({ status: 502, body: "" });
        }
    }
    return route.fulfill({ status: 200, contentType: "text/javascript; charset=utf-8", body: chunkCache.get(url) });
});

await page.goto(`http://localhost:${port}/`);
// Wait for the chart to actually exist and hold data.
await page.waitForFunction(() => !!document.querySelector("timeline-view"), null, { timeout: 20000 });
await page.waitForTimeout(3000);
// Phase 2: the chart the user actually watches — loaded, live-polling,
// following "now". Reset and measure a settled window.
const loadPhase = await page.evaluate(() => {
    // Attribute the three worst load frames: which component calls ran inside
    // each, and for how long.
    const worst = window.__rafs.slice().sort((a, b) => b.ms - a.ms).slice(0, 3);
    const log = window.__innerLog ?? [];
    const detail = worst.map((f) => ({
        frame: f.ms,
        inside: log.filter((e) => e.at >= f.at - 1 && e.at <= f.at + f.ms)
                   .sort((a, b) => b.ms - a.ms).slice(0, 6)
                   .map((e) => `${e.name} ${e.ms}ms`),
    }));
    const out = { rafMax: window.__rafMax, rafs: window.__rafs.slice().sort((a, b) => b.ms - a.ms).slice(0, 5), detail };
    window.__rafMax = 0; window.__rafs = []; window.__gaps = []; window.__settled = true;
    return out;
});
console.log(`LOAD BURST worst rAF ${loadPhase.rafMax.toFixed(2)} ms | top: ${loadPhase.rafs.map((r) => r.ms + "ms").join(", ")}`);
for (const d of loadPhase.detail) console.log(`   frame ${d.frame}ms <= ${d.inside.join(" | ")}`);
await page.waitForTimeout(4000);
const settled = await page.evaluate(() => ({
    rafMax: window.__rafMax,
    rafs: window.__rafs.slice().sort((a, b) => b.ms - a.ms).slice(0, 5),
}));
console.log(`SETTLED worst rAF ${settled.rafMax.toFixed(2)} ms | top: ${settled.rafs.map((r) => r.ms + "ms").join(", ") || "all under 1ms"}`);

// Phase 3: INTERACTION on the loaded chart — pan and zoom, the other way a
// full redraw happens. "Never blocks a frame" has to hold here too.
await page.evaluate(() => { window.__rafMax = 0; window.__rafs = []; });
await page.evaluate(async () => {
    const tl = document.querySelector("timeline-view");
    const wait = () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    const now = Date.now();
    tl.followNow = false;
    for (let i = 0; i < 24; i++) { // zoom out in steps, then pan across
        const span = 5 * 60000 * (1 + i);
        tl.setViewport(now - span, now);
        await wait();
    }
    for (let i = 0; i < 24; i++) {
        const off = i * 60000;
        tl.setViewport(now - 3600000 - off, now - off);
        await wait();
    }
});
const interact = await page.evaluate(() => ({
    rafMax: window.__rafMax,
    rafs: window.__rafs.slice().sort((a, b) => b.ms - a.ms).slice(0, 5),
}));
console.log(`INTERACTION worst rAF ${interact.rafMax.toFixed(2)} ms | top: ${interact.rafs.map((r) => r.ms + "ms").join(", ")}`);

const result = await page.evaluate(() => ({
    worstTask: window.__worst,
    tasks: window.__tasks.slice().sort((a, b) => b - a).slice(0, 5),
    gaps: window.__gaps.slice().sort((a, b) => b.gap - a.gap).slice(0, 6),
    ingest: window.__ingest.slice().sort((a, b) => b.ms - a.ms).slice(0, 6),
    loaf: window.__loaf.slice().sort((a, b) => b.dur - a.dur).slice(0, 6),
    loafErr: window.__loafErr ?? null,
    rafMax: window.__rafMax,
    rafs: window.__rafs.slice().sort((a, b) => b.ms - a.ms).slice(0, 6),
    inner: window.__inner ?? {},
    ingestCalls: window.__ingest.length,
    ingestTotal: window.__ingest.reduce((a, b) => a + b.ms, 0),
    intervals: document.querySelector("timeline-view") ? "rendered" : "missing",
    worstSlice: document.querySelector("gsm-timeline")?.worstSliceMs ?? -1,
}));

console.log(`chart: ${result.intervals}`);
console.log(`worst decode slice (element's own count): ${result.worstSlice.toFixed?.(2) ?? result.worstSlice} ms`);
console.log(`longest long-tasks (ms): ${result.tasks.map((n) => n.toFixed(1)).join(", ") || "none over the 50ms reporting floor"}`);
console.log(`longest frame gaps: ${result.gaps.map((g) => `${g.gap.toFixed(1)}ms @${g.at}`).join(", ") || "none over 20ms"}`);
if (result.loafErr) console.log(`long-animation-frame unavailable: ${result.loafErr}`);
for (const f of result.loaf) {
    console.log(`  LoAF ${f.dur}ms (blocking ${f.blocking}ms, render starts +${f.renderStart}ms): ` +
        f.scripts.map((s) => `${s.src} [${s.invoker}] ${s.dur}ms`).join(" | "));
}
console.log(`worst rAF callback (component draw + ours): ${result.rafMax.toFixed(2)} ms`);
console.log(`  top rAF callbacks: ${result.rafs.map((r) => `${r.ms}ms @${r.at}`).join(", ")}`);
console.log("component internals (self+children, ms):");
for (const [k, v] of Object.entries(result.inner).sort((a, b) => b[1].total - a[1].total).slice(0, 12)) {
    console.log(`   ${k.padEnd(20)} calls ${String(v.calls).padStart(5)}  total ${v.total.toFixed(0).padStart(5)}ms  max ${v.max.toFixed(1)}ms`);
}
console.log(`component ingest: ${result.ingestCalls} calls, ${result.ingestTotal.toFixed(1)}ms total`);
for (const c of result.ingest) console.log(`   ${c.name}(${c.n}) ${c.ms.toFixed(1)}ms @${c.at}`);

// The number to assert on: the worst main-thread block we can see, from
// whichever source saw it.
const worst = Math.max(result.worstTask, result.worstSlice,
    ...(result.gaps.length ? [result.gaps[0].gap - 16.7] : [0]));
console.log(`WORST_TASK_MS ${worst.toFixed(3)}`);

await browser.close();
server.close();
