package api

import _ "embed"

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

//go:embed testdata/node-request-log.js
var nodeRequestLog string

// TestRequestLogModel runs the built request-log module's buffer under node:
// grouping, the stable order, the bound, the filter, and its cost when full.
func TestRequestLogModel(t *testing.T) {
	module, err := filepath.Abs("web/assets/request-log.js")
	require.NoError(t, err)
	_, err = os.Stat(module)
	require.NoError(t, err, "run `just build` first")
	_, err = exec.LookPath("node")
	require.NoError(t, err, "node drives the browser module")

	script := filepath.Join(t.TempDir(), "request-log.mjs")
	require.NoError(t, os.WriteFile(script,
		[]byte(strings.Replace(nodeRequestLog, "%s", "file://"+module, 1)), 0o644))

	cmd := exec.Command("node", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "the model failed:\n%s", stderr.String())
	require.Contains(t, string(out), `"ok":true`, "the harness did not reach its end: %s", out)
	t.Log(string(out))
}
