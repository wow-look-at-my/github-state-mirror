// Browser-side half of the comparison: what the DASHBOARD pays to turn a
// payload into the chart's interval objects. Node's V8 is the same engine
// Chrome runs, so JSON.parse here is the same native C++ parse the browser
// does — which is exactly why a hand-rolled format has to beat a native
// parser written in JS to be worth shipping.
//
// Decompression is deliberately NOT measured on either side: whatever we pick
// rides Content-Encoding, so the browser inflates it in C++ off the main
// thread before any of this runs.
import { readFileSync } from "node:fs";

const N = 100000;
const REPS = 5;

function bench(name, fn) {
    let best = Infinity, out;
    for (let i = 0; i < REPS; i++) {
        const t0 = process.hrtime.bigint();
        out = fn();
        const ms = Number(process.hrtime.bigint() - t0) / 1e6;
        if (ms < best) best = ms;
    }
    console.log(`${name.padEnd(38)} ${best.toFixed(1).padStart(8)} ms   ${String(out).padStart(9)}`);
    return best;
}

// ---- the chart's mapping (internal/api/web/src/timeline.ts) ---------------

function labelFor(e) {
    if (e.kind === "webhook") {
        if (e.repo) { const s = e.repo.indexOf("/"); return s >= 0 ? e.repo.slice(s + 1) : e.repo; }
        return e.action ?? "";
    }
    return e.status ? String(e.status) : "";
}

function toInterval(e) {
    const start = Date.parse(e.start);
    return { id: String(e.id), laneId: e.lane, start, end: start + Math.max(0, e.dur_ms),
             label: labelFor(e), category: e.lane, state: e.disposition, data: e };
}

// ---- hand-rolled readers -------------------------------------------------

class Reader {
    constructor(buf) { this.b = buf; this.p = 0; this.dec = new TextDecoder(); }
    uvarint() {
        let x = 0, s = 0, b = this.b;
        for (;;) { const c = b[this.p++]; if (c < 0x80) return x + c * 2 ** s; x += (c & 0x7f) * 2 ** s; s += 7; }
    }
    varint() { const u = this.uvarint(); return (u % 2 === 0) ? u / 2 : -(u + 1) / 2; }
    str() { const n = this.uvarint(); const s = this.dec.decode(this.b.subarray(this.p, this.p + n)); this.p += n; return s; }
    dict() { const n = this.uvarint(); const out = new Array(n); for (let i = 0; i < n; i++) out[i] = this.str(); return out; }
    magic(want) {
        const got = String.fromCharCode(...this.b.subarray(0, 4));
        if (got !== want) throw new Error(`bad magic ${got}`);
        this.p = 4;
    }
}

const COLS = ["kind", "lane", "disposition", "event_type", "action", "delivery_id",
              "repo", "method", "route", "actor", "actor_name", "detail", "target"];

// decodeColumns: the columns themselves, no per-event object anywhere.
function decodeColumns(buf) {
    const r = new Reader(buf);
    r.magic("TLC1");
    const n = r.uvarint();
    const id = new Float64Array(n), start = new Float64Array(n), dur = new Int32Array(n),
          status = new Int32Array(n), attempt = new Int32Array(n);
    let acc = 0;
    for (let i = 0; i < n; i++) { acc += r.uvarint(); id[i] = acc; }
    let ms = 0;
    for (let i = 0; i < n; i++) { ms += r.varint(); start[i] = ms; }
    for (let i = 0; i < n; i++) dur[i] = r.uvarint();
    for (let i = 0; i < n; i++) status[i] = r.uvarint();
    for (let i = 0; i < n; i++) attempt[i] = r.uvarint();
    const bits = buf.subarray(r.p, r.p + ((n + 7) >> 3)); r.p += (n + 7) >> 3;
    const cols = {};
    for (const name of COLS) {
        const d = r.dict();
        if (d.length === 1) { cols[name] = { dict: d, idx: null }; continue; }
        const idx = new Int32Array(n);
        for (let i = 0; i < n; i++) idx[i] = r.uvarint();
        cols[name] = { dict: d, idx };
    }
    return { n, id, start, dur, status, attempt, bits, cols };
}

const colStr = (c, i) => (c.idx === null ? "" : c.dict[c.idx[i]]);

// Path A: columnar -> the same interval objects, events materialized eagerly
// (drop-in for today's client, `data` still a full event object).
function colToIntervalsEager(buf) {
    const c = decodeColumns(buf);
    const out = new Array(c.n);
    for (let i = 0; i < c.n; i++) {
        const e = { id: c.id[i], kind: colStr(c.cols.kind, i), lane: colStr(c.cols.lane, i),
            start: c.start[i], dur_ms: c.dur[i], disposition: colStr(c.cols.disposition, i),
            event_type: colStr(c.cols.event_type, i), action: colStr(c.cols.action, i),
            delivery_id: colStr(c.cols.delivery_id, i), repo: colStr(c.cols.repo, i),
            method: colStr(c.cols.method, i), route: colStr(c.cols.route, i), status: c.status[i],
            actor: colStr(c.cols.actor, i), actor_name: colStr(c.cols.actor_name, i),
            detail: colStr(c.cols.detail, i), target: colStr(c.cols.target, i),
            attempt: c.attempt[i], final: (c.bits[i >> 3] & (1 << (i & 7))) !== 0 };
        const s = c.start[i];
        out[i] = { id: String(e.id), laneId: e.lane, start: s, end: s + Math.max(0, e.dur_ms),
                   label: labelFor(e), category: e.lane, state: e.disposition, data: e };
    }
    return out;
}

// Path B: columnar -> intervals with NO event objects at all. The tooltip
// needs one event, on hover; the chart needs 100k intervals, now.
function colToIntervalsLazy(buf) {
    const c = decodeColumns(buf);
    const out = new Array(c.n);
    const kind = c.cols.kind, lane = c.cols.lane, repo = c.cols.repo,
          action = c.cols.action, disp = c.cols.disposition;
    for (let i = 0; i < c.n; i++) {
        const s = c.start[i], ln = colStr(lane, i);
        let label;
        if (colStr(kind, i) === "webhook") {
            const rp = colStr(repo, i);
            label = rp ? rp.slice(rp.indexOf("/") + 1) : colStr(action, i);
        } else {
            label = c.status[i] ? String(c.status[i]) : "";
        }
        out[i] = { id: String(c.id[i]), laneId: ln, start: s, end: s + Math.max(0, c.dur[i]),
                   label, category: ln, state: colStr(disp, i), row: i };
    }
    return out;
}

// Path C: the row format, for completeness.
function rowToIntervals(buf) {
    const r = new Reader(buf);
    r.magic("TLR1");
    const n = r.uvarint();
    const d = r.dict();
    const out = new Array(n);
    let id = 0, ms = 0;
    for (let i = 0; i < n; i++) {
        id += r.uvarint(); ms += r.varint();
        const dur = r.uvarint(), mask = r.uvarint();
        const e = { id, start: ms, dur_ms: dur, kind: d[r.uvarint()], lane: d[r.uvarint()] };
        // bit order matches codec_row.go
        const names = ["disposition", "event_type", "action", "delivery_id", "repo", "method",
                       "route", null, "actor", "actor_name", "detail", "target"];
        for (let b = 0, ni = 0; b < 12; b++) {
            if (names[b] === null) continue;
            if (mask & (1 << b)) e[names[b]] = d[r.uvarint()];
            ni++;
        }
        if (mask & (1 << 7)) e.status = r.uvarint();
        if (mask & (1 << 12)) e.attempt = r.uvarint();
        e.final = (mask & (1 << 13)) !== 0;
        out[i] = { id: String(e.id), laneId: e.lane, start: e.start, end: e.start + dur,
                   label: labelFor(e), category: e.lane, state: e.disposition, data: e };
    }
    return out;
}

// ---- run -----------------------------------------------------------------

const jsonBytes = readFileSync("events.json");
const jsonText = jsonBytes.toString("utf8");
const col = new Uint8Array(readFileSync("events.col"));
const row = new Uint8Array(readFileSync("events.row"));

console.log(`payloads: json ${(jsonBytes.length / 1048576).toFixed(2)} MB, columnar ${(col.length / 1048576).toFixed(2)} MB, row ${(row.length / 1048576).toFixed(2)} MB\n`);
console.log(`${"path".padEnd(38)} ${"best of " + REPS} ms   intervals`);

bench("JSON.parse only", () => JSON.parse(jsonText).length);
bench("JSON.parse + map to intervals", () => JSON.parse(jsonText).map(toInterval).length);
bench("columnar decode (columns only)", () => decodeColumns(col).n);
bench("columnar -> intervals (eager events)", () => colToIntervalsEager(col).length);
bench("columnar -> intervals (lazy detail)", () => colToIntervalsLazy(col).length);
bench("row -> intervals", () => rowToIntervals(row).length);

// Correctness: the three paths must agree on what the chart draws.
const a = JSON.parse(jsonText).map(toInterval), b = colToIntervalsEager(col),
      c = colToIntervalsLazy(col), e = rowToIntervals(row);
let bad = 0;
for (let i = 0; i < N; i++) {
    for (const [x, y] of [[a[i], b[i]], [a[i], c[i]], [a[i], e[i]]]) {
        if (x.id !== y.id || x.laneId !== y.laneId || x.start !== y.start || x.end !== y.end ||
            x.label !== y.label || x.state !== y.state) bad++;
    }
}
console.log(`\ninterval mismatches vs JSON path: ${bad}`);
