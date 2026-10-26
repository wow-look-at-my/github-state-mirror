
// The module registers a custom element at import time; stub the two DOM
// globals that costs so the shipped file needs no test-only accommodation.
globalThis.HTMLElement = class {};
globalThis.customElements = { define() {} };

// A response the SERVER legitimately produces — this is exactly what
// handleTimeline answers a caller that did not name the wire type.
const jsonBody = JSON.stringify({ events: [], max_id: 0, retention_start: "", now: "" });
let sentAccept = null;
globalThis.fetch = async (_url, init) => {
    sentAccept = init?.headers?.Accept ?? null;
    return {
        ok: true,
        headers: { get: (k) => (k.toLowerCase() === "content-type" ? "application/json" : null) },
        json: async () => JSON.parse(jsonBody),
        arrayBuffer: async () => new TextEncoder().encode(jsonBody).buffer,
    };
};

const { fetchTimelineBytes } = await import("%s");
let threw = false;
try {
    await fetchTimelineBytes("/api/timeline");
} catch (e) {
    threw = true;
}
process.stdout.write(JSON.stringify({ threw, sentAccept }));
