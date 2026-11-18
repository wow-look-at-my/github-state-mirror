// Drives the BUILT request-log module's model under node.
import assert from "node:assert/strict";

const { RequestLogModel } = await import("%s");

let seq = 0;
const ev = (path, over = {}) => ({
    actor: "user:1", actor_name: "octocat", method: "GET", path, disposition: "hit",
    at: new Date(1700000000000 + seq * 1000).toISOString(), seq: ++seq, ...over,
});
const all = { grouped: true, sort: "first", filter: "", hidden: new Set(), limit: 100 };

// Identical requests fold into one group that counts them.
{
    seq = 0;
    const m = new RequestLogModel(100);
    assert.equal(m.ingest([ev("/a"), ev("/a"), ev("/b"), ev("/a")]), 4);
    const r = m.query(all);
    assert.equal(r.matched, 2);
    assert.deepEqual(r.groups.map((g) => [g.sample.path, g.count]), [["/b", 1], ["/a", 3]], "newest kind first");
    assert.equal(r.groups[1].lastSeq, 4);
    assert.equal(r.groups[1].times.length, 3);
}

// A different caller, result or status is a different group.
{
    seq = 0;
    const m = new RequestLogModel(100);
    m.ingest([ev("/a"), ev("/a", { disposition: "miss" }), ev("/a", { actor: "app:9" }), ev("/a", { status: 404 })]);
    assert.equal(m.groupCount, 4);
}

// The default order never moves a row: a busy group stays where it first appeared.
{
    seq = 0;
    const m = new RequestLogModel(100);
    m.ingest([ev("/old"), ev("/new")]);
    for (let i = 0; i < 20; i++) m.ingest([ev("/old")]);
    assert.deepEqual(m.query(all).groups.map((g) => g.sample.path), ["/new", "/old"]);
    assert.deepEqual(m.query({ ...all, sort: "last" }).groups.map((g) => g.sample.path), ["/old", "/new"]);
    assert.deepEqual(m.query({ ...all, sort: "count" }).groups.map((g) => g.sample.path), ["/old", "/new"]);
}

// The same event twice is counted once: the stream and a fetch overlap.
{
    seq = 0;
    const m = new RequestLogModel(100);
    const batch = [ev("/a"), ev("/a")];
    m.ingest(batch);
    assert.equal(m.ingest(batch), 0);
    assert.equal(m.query(all).groups[0].count, 2);
}

// The buffer is bounded, and a group whose events all left goes with them.
{
    seq = 0;
    const m = new RequestLogModel(5);
    m.ingest([ev("/gone"), ev("/stays")]);
    for (let i = 0; i < 5; i++) m.ingest([ev("/fill")]);
    assert.equal(m.size, 5);
    assert.deepEqual(m.query(all).groups.map((g) => [g.sample.path, g.count]), [["/fill", 5]]);
    assert.equal(m.groupCount, 1);
}

// History that arrives after live events goes in front of them.
{
    seq = 0;
    const older = [ev("/history"), ev("/history"), ev("/live")];
    const live = [ev("/live"), ev("/live")];
    const m = new RequestLogModel(100);
    m.ingest(live);
    assert.equal(m.ingest([...older].reverse()), 3, "any order is accepted");
    assert.equal(m.size, 5);
    const r = m.query(all);
    assert.deepEqual(r.groups.map((g) => [g.sample.path, g.count, g.firstSeq]), [["/live", 3, 3], ["/history", 2, 1]]);
    const flat = m.query({ ...all, grouped: false });
    assert.deepEqual(flat.events.map((e) => e.seq), [5, 4, 3, 2, 1], "the flat view is newest first");
}

// The filter matches every term, in any field, and a hidden result is left out.
{
    seq = 0;
    const m = new RequestLogModel(100);
    m.ingest([
        ev("/repos/o/r/pulls/1"),
        ev("/repos/o/r/commits", { disposition: "passthrough", status: 502, reason: "unmodeled-query" }),
        ev("/graphql", { method: "POST", actor_name: "pr-minder" }),
    ]);
    const paths = (q) => m.query({ ...all, ...q }).groups.map((g) => g.sample.path);
    assert.deepEqual(paths({ filter: "REPOS pulls" }), ["/repos/o/r/pulls/1"]);
    assert.deepEqual(paths({ filter: "502" }), ["/repos/o/r/commits"]);
    assert.deepEqual(paths({ filter: "pr-minder post" }), ["/graphql"]);
    assert.deepEqual(paths({ filter: "unmodeled" }), ["/repos/o/r/commits"]);
    assert.deepEqual(paths({ hidden: new Set(["hit"]) }), ["/repos/o/r/commits"]);
    assert.equal(m.query({ ...all, grouped: false, filter: "repos" }).matched, 2);
}

// The limit bounds the rows and still reports how many matched.
{
    seq = 0;
    const m = new RequestLogModel(1000);
    for (let i = 0; i < 300; i++) m.ingest([ev("/p/" + i)]);
    const r = m.query({ ...all, limit: 100 });
    assert.equal(r.groups.length, 100);
    assert.equal(r.matched, 300);
}

// Clear empties the log and keeps the cursor, so cleared events never return.
{
    seq = 0;
    const m = new RequestLogModel(100);
    const batch = [ev("/a"), ev("/b")];
    m.ingest(batch);
    m.clear();
    assert.equal(m.size, 0);
    assert.equal(m.maxSeq, 2);
    assert.equal(m.ingest(batch), 0);
    assert.equal(m.ingest([ev("/c")]), 1);
    m.reset();
    assert.equal(m.maxSeq, 0, "a server restart starts the cursor again");
    assert.equal(m.ingest(batch), 2);
}

// A full buffer under sustained traffic stays cheap: this is the "do not lag the browser" bound.
{
    seq = 0;
    const m = new RequestLogModel(20000);
    const t0 = performance.now();
    for (let i = 0; i < 2000; i++) {
        const batch = [];
        for (let j = 0; j < 50; j++) batch.push(ev("/repos/o/r/commits/" + ((i * 50 + j) % 30000)));
        m.ingest(batch);
    }
    const ingestMs = performance.now() - t0;
    assert.equal(m.size, 20000);
    const q0 = performance.now();
    for (const sort of ["first", "last", "count"]) m.query({ ...all, sort, filter: "commits 1" });
    m.query({ ...all, grouped: false, filter: "commits 1" });
    const queryMs = (performance.now() - q0) / 4;
    assert.ok(ingestMs < 5000, `100,000 events took ${ingestMs.toFixed(0)}ms to ingest`);
    assert.ok(queryMs < 250, `one filtered query over a full buffer took ${queryMs.toFixed(1)}ms`);
    process.stdout.write(JSON.stringify({ ok: true, ingestMs: Math.round(ingestMs), queryMs: Math.round(queryMs * 10) / 10 }));
}
