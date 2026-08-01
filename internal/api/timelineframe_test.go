package api

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// THE FRAME BUDGET, AS A CHECK.
//
// Operator requirement: the chart's decode "must not ever block for more than
// 8ms per frame". That is a property of the SHIPPED browser module, so prose
// cannot hold it — the first cut of the columnar decoder blocked for 63 ms in
// one task and nothing failed. This test drives the real built
// assets/timeline.js under node over the payload the dashboard actually
// fetches on first paint, and fails on the longest synchronous stretch.
//
// What makes the number meetable at all is upstream of the slicing: the chart
// FETCHES ONE HOUR (INITIAL_WINDOW_MS) and pulls older ranges through the
// component's async history loader. A full 24h window is 100k live interval
// objects, and the GC pauses that many survivors cost (measured: 4-7 ms
// scavenges landing mid-decode) are not something cooperative yielding can
// hide — the fix has to be not holding them.

var worstSliceRe = regexp.MustCompile(`WORST_SLICE_MS ([0-9.]+)`)

func TestTimelineFrameBudget(t *testing.T) {
	module, err := filepath.Abs("web/assets/timeline.js")
	require.NoError(t, err)
	if _, err := os.Stat(module); err != nil {
		t.Skip("run `npm run build` first: " + module + " is missing")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed — cannot measure the browser decoder")
	}
	harness, err := filepath.Abs("../../prototype/timelinewire/framecheck.mjs")
	require.NoError(t, err)

	// Exactly what the first paint asks for: the last hour of realistic
	// traffic out of a full ring.
	ring := realisticRing(100000, 24*time.Hour)
	hour := ring.SnapshotRange(time.Now().UTC().Add(-time.Hour), time.Time{})
	require.Greater(t, len(hour.Events), 1000, "the fixture must be big enough to be a real test")

	payload := filepath.Join(t.TempDir(), "timeline-1h.bin")
	require.NoError(t, os.WriteFile(payload, encodeTimelineV1(hour), 0o644))

	cmd := exec.Command("node", harness, payload)
	cmd.Env = append(os.Environ(), "GSM_TIMELINE_JS="+module)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "frame-budget harness failed:\n%s", stderr.String())

	m := worstSliceRe.FindSubmatch(out)
	require.NotNil(t, m, "harness printed no WORST_SLICE_MS:\n%s", out)
	worst, err := strconv.ParseFloat(string(m[1]), 64)
	require.NoError(t, err)

	t.Logf("first paint: %d events, worst synchronous slice %.2f ms (budget %d ms)",
		len(hour.Events), worst, frameBudgetMs)
	require.LessOrEqual(t, worst, float64(frameBudgetMs),
		"decoding the first-paint window blocked the main thread for %.2f ms — "+
			"the chart drops frames. Either a phase stopped yielding, or the "+
			"initial window grew.", worst)
}

// frameBudgetMs mirrors FRAME_BUDGET_MS in web/src/timeline.ts. Kept as a
// literal on both sides on purpose: the TS constant is what the element warns
// against at runtime, this is what CI fails on, and they are meant to be read
// together.
const frameBudgetMs = 8
