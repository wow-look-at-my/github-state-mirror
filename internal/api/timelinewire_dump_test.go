package api

import (
	"encoding/json"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// TestTimelineWireDumpPayloads writes a realistic FULL-RING window in both
// encodings, for measuring the browser side against the SHIPPED decoder:
//
//	GSM_DUMP=/tmp go test./internal/api -run TestTimelineWireDumpPayloads -v
//
// It is a measurement fixture, not an assertion — inert unless GSM_DUMP is
// set. The size claim itself is asserted by TestTimelineWireIsMuchSmallerThanJSON.
func TestTimelineWireDumpPayloads(t *testing.T) {
	dir := os.Getenv("GSM_DUMP")
	if dir == "" {
		t.Skip("set GSM_DUMP=<dir> to write the measurement payloads")
	}
	tl := realisticRing(100000, 24*time.Hour)
	// The full ring, plus the -hour window the chart paints on load — its decode has a frame budget.
	hour := tl.SnapshotRange(time.Now().UTC().Add(-time.Hour), time.Time{})
	hourWire := mustEncodeTimeline(t, hour)
	require.NoError(t, os.WriteFile(dir+"/timeline-1h.bin", hourWire, 0o644))

	t.Logf("1h window: %d events, %d B", len(hour.Events), len(hourWire))
	// A shorter window, measuring how the chart's load burst scales with what is on screen (browsercheck.ts).
	short := tl.SnapshotRange(time.Now().UTC().Add(-20*time.Minute), time.Time{})
	shortWire := mustEncodeTimeline(t, short)
	require.NoError(t, os.WriteFile(dir+"/timeline-20m.bin", shortWire, 0o644))
	t.Logf("20m window: %d events, %d B", len(short.Events), len(shortWire))

	snap := tl.Snapshot(0)
	wire := mustEncodeTimeline(t, snap)
	require.NoError(t, os.WriteFile(dir+"/timeline.bin", wire, 0o644))

	// The retired JSON shape; kept only so the bench can compare its decoder against columnar.
	body, _ := json.Marshal(legacyTimelineJSON{Events: snap.Events, MaxID: snap.MaxID,
		RetentionStart: snap.RetentionStart.UTC().Format(time.RFC3339Nano),
		Now:            snap.Now.UTC().Format(time.RFC3339Nano)})
	require.NoError(t, os.WriteFile(dir+"/timeline.json", body, 0o644))

	t.Logf("100k events: columnar %d B (%.1f B/event), json %d B (%.1f B/event)",
		len(wire), float64(len(wire))/100000, len(body), float64(len(body))/100000)
}

// legacyTimelineJSON is the retired response shape, kept only as a bench
// fixture (see above).
type legacyTimelineJSON struct {
	Events         []reqtimeline.Event `json:"events"`
	MaxID          uint64              `json:"max_id"`
	RetentionStart string              `json:"retention_start"`
	Now            string              `json:"now"`
}

// realisticRing builds a ring shaped like real mirror traffic: mostly requests
// across a handful of route shapes, ~% webhook deliveries carrying UNIQUE
// delivery GUIDs (the field no dictionary can compress — leaving them out
// would flatter the format), a few outbound notifications. Deterministic.
func realisticRing(n int, span time.Duration) *reqtimeline.Recorder {
	rng := rand.New(rand.NewSource(7))
	step := span / time.Duration(n)
	routes := []string{"/repos/{owner}/{repo}/pulls", "/repos/{owner}/{repo}/commits",
		"/repos/{owner}/{repo}/compare/{basehead}", "/repos/{owner}/{repo}/contents/{path}",
		"/repos/{owner}/{repo}/actions/runs", "/graphql", "/repos/{owner}/{repo}/branches"}
	types := []string{"push", "pull_request", "check_run", "check_suite", "status", "workflow_job"}
	actors := []string{"user:11347985", "app:1352104", "app-installation:40000000", "app-installation:40000137"}
	tl := reqtimeline.New()
	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * step)
		switch r := rng.Intn(100); {
		case r < 84:
			tl.RecordRequest(at, time.Duration(rng.Intn(400))*time.Millisecond, "GET",
				routes[rng.Intn(len(routes))], []int{200, 200, 200, 404, 502}[rng.Intn(5)],
				[]string{DispHit, DispMiss, DispPassthrough, dispUpstream, dispInternal}[rng.Intn(5)],
				actors[rng.Intn(len(actors))], "installation-account")
		case r < 95:
			guid := make([]byte, 36)
			for j := range guid {
				guid[j] = "0123456789abcdef"[rng.Intn(16)]
			}
			guid[8], guid[13], guid[18], guid[23] = '-', '-', '-', '-'
			tl.RecordWebhook(at, 3*time.Millisecond, types[rng.Intn(len(types))], "opened",
				string(guid), "wow-look-at-my/repo-"+strconv.Itoa(rng.Intn(220)), "applied")
		default:
			tl.RecordNotify(at, 120*time.Millisecond, "webhook-runner.pazer.io", 200, 1, true, "applied")
		}
	}
	return tl
}
