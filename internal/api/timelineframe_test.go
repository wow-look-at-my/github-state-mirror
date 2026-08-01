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
// Operator requirement: the loader runs ONE chunk per frame, and the average
// main-thread work per frame across the load stays under 8 ms. That is a
// property of the SHIPPED browser module, so prose cannot hold it — the first
// cut of the columnar decoder blocked for 63 ms in one task and nothing
// failed. This test drives the real built assets/timeline.js under node over
// the payload the dashboard actually fetches on first paint.
//
// The AVERAGE is the gate because it is the requirement. The worst single
// chunk is logged and bounded loosely: a chunk is sized by what the previous
// one cost, so a GC pause landing inside one shows up there and is not a
// sizing defect — but a chunk twice the budget is, and that is what the loose
// bound catches.
//
// What makes any of this reachable is upstream of the slicing: the chart
// FETCHES ONE HOUR (INITIAL_WINDOW_MS) and pulls older ranges through the
// component's async history loader. A full 24h window is 100k live interval
// objects, and the GC pauses that many survivors cost (measured: 4-7 ms
// scavenges landing mid-decode) are not something cooperative yielding can
// hide — the fix has to be not holding them.

var (
	worstSliceRe = regexp.MustCompile(`WORST_SLICE_MS ([0-9.]+)`)
	avgFrameRe   = regexp.MustCompile(`AVG_FRAME_MS ([0-9.]+)`)
)

func TestTimelineFrameBudget(t *testing.T) {
	module, err := filepath.Abs("web/assets/timeline.js")
	require.NoError(t, err)
	if _, err := os.Stat(module); err != nil {
		t.Skip("run `npm run build` first: " + module + " is missing")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed — cannot measure the browser decoder")
	}
	harness, err := filepath.Abs("testdata/framecheck.mjs")
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

	worst := parseHarnessMs(t, out, worstSliceRe, "WORST_SLICE_MS")
	avg := parseHarnessMs(t, out, avgFrameRe, "AVG_FRAME_MS")

	t.Logf("first paint: %d events, %.2f ms average per frame, worst chunk %.2f ms (budget %d ms)",
		len(hour.Events), avg, worst, frameBudgetMs)

	require.LessOrEqual(t, avg, float64(frameBudgetMs),
		"loading the first-paint window averaged %.2f ms of main-thread work per "+
			"frame — the chart drops frames for the whole load. Either a chunk "+
			"stopped sizing itself, or the initial window grew.", avg)
	require.LessOrEqual(t, worst, float64(2*frameBudgetMs),
		"one chunk cost %.2f ms — more than double the frame budget, so chunk "+
			"sizing is not tracking what the work actually costs.", worst)
}

func parseHarnessMs(t *testing.T, out []byte, re *regexp.Regexp, label string) float64 {
	t.Helper()
	m := re.FindSubmatch(out)
	require.NotNil(t, m, "harness printed no %s:\n%s", label, out)
	v, err := strconv.ParseFloat(string(m[1]), 64)
	require.NoError(t, err)
	return v
}

// frameBudgetMs mirrors FRAME_BUDGET_MS in web/src/timeline.ts. Kept as a
// literal on both sides on purpose: the TS constant is what chunks are sized
// against, this is what CI fails on, and they are meant to be read together.
const frameBudgetMs = 8
