package api

import (
	"bytes"
	"encoding/json"
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
// names its media type, readable JSON to everyone else. The JSON is only safe
// because the CHART REFUSES IT — an earlier version took whatever came back, so
// an Accept that drifted silently cost ~10x the frames with nothing logged.
//
// The refusal is therefore what lets the JSON exist at all, so this drives the
// REAL BUILT module under node against a stubbed fetch rather than trusting a
// comment to hold it.

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

const { fetchTimelineBytes } = await import("%s");
let threw = false;
try {
    await fetchTimelineBytes("/api/timeline");
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
		"fetchTimelineBytes ACCEPTED a JSON answer (%s) — the chart would silently take "+
			"the ~10x-slower decode. The endpoint may serve JSON to curl only "+
			"because the client refuses it.", got)
	var result struct {
		SentAccept string `json:"sentAccept"`
	}
	require.NoError(t, json.Unmarshal(out, &result), "harness output must be JSON: %s", got)
	require.Equal(t, timelineWireType, result.SentAccept,
		"the client must ask for the wire type by exact media type, got %s", got)
}
