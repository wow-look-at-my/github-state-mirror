package api

import _ "embed"

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

//go:embed testdata/node-client-strict.js
var nodeClientStrict string

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
