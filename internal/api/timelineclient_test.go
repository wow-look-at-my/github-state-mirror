package api

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// THE CLIENT MUST NOT FALL BACK.
//
// /api/timeline serves two encodings: the columnar payload to a caller that
// names its media type, readable JSON to everyone else (curl, jq, a browser).
// That second encoding is only safe because the CHART REFUSES IT. An earlier
// version accepted whatever came back, so an Accept that drifted — a header
// edit, a proxy rewriting it, a q= mixup — silently took a decode costing ~10x
// the frames, with nothing failing and nothing logged.
//
// The refusal therefore is not defensive tidiness; it is what lets the JSON
// exist at all. Prose cannot hold it, so this drives the REAL BUILT module
// under node with a stubbed fetch and requires fetchDecoded to reject a JSON
// answer.

const nodeClientStrict = `
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

const { fetchDecoded } = await import("%s");
let threw = false;
try {
    await fetchDecoded("/api/timeline");
} catch (e) {
    threw = true;
}
process.stdout.write(JSON.stringify({ threw, sentAccept }));
`

func TestTimelineClientRefusesJSON(t *testing.T) {
	module, err := filepath.Abs("web/assets/timeline.js")
	require.NoError(t, err)
	if _, err := os.Stat(module); err != nil {
		t.Skip("run `npm run build` first: " + module + " is missing")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed — cannot drive the browser module")
	}

	script := filepath.Join(t.TempDir(), "strict.mjs")
	require.NoError(t, os.WriteFile(script,
		[]byte(strings.Replace(nodeClientStrict, "%s", "file://"+module, 1)), 0o644))

	cmd := exec.Command("node", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "harness failed:\n%s", stderr.String())

	got := string(out)
	require.Contains(t, got, `"threw":true`,
		"fetchDecoded ACCEPTED a JSON answer (%s) — the chart would silently take "+
			"the ~10x-slower decode. The endpoint may serve JSON to curl only "+
			"because the client refuses it.", got)
	require.Contains(t, got, `"sentAccept":"`+timelineWireType+`"`,
		"the client must ask for the wire type by exact media type, got %s", got)
}
