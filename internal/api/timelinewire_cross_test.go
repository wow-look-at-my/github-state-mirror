package api

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The columnar format has two decoders in two languages — Go's (the spec
// mirror in timelinewire_test.go) and the browser's, in
// internal/api/web/src/timeline.ts. A format is only as good as the agreement
// between them, and a Go-only round-trip proves nothing about the half that
// actually runs: a wrong shift, a rune-vs-byte length, a missed empty column
// would all pass the Go test and paint a broken chart.
//
// So this test runs the REAL BUILT dashboard module — assets/timeline.js, the
// exact bytes the server embeds — under node, feeds it a payload this package
// encoded, and compares every field of every event against what Go put in.
// (timeline.ts exports pageFromWire/eventAt for this; the DOM globals its
// custom-element registration needs are stubbed in the harness below, so the
// shipped module stays free of test-only accommodation.)

// nodeCrossCheck is the script run against the built module. It prints the
// decoded events as JSON on stdout.
const nodeCrossCheck = `
// The module registers a custom element at import time; stub the two DOM
// globals that costs so the shipped file needs no test-only accommodation.
globalThis.HTMLElement = class {};
globalThis.customElements = { define() {} };
const { pageFromWire, eventAt } = await import("%s");
import { readFileSync } from "node:fs";
const page = pageFromWire(new Uint8Array(readFileSync("%s")));
process.stdout.write(JSON.stringify({
    max_id: page.maxId,
    retention_start: page.retentionStart,
    now: page.now,
    lanes: page.lanes,
    intervals: page.intervals.map((iv) => ({
        id: iv.id, laneId: iv.laneId, start: iv.start, end: iv.end,
        label: iv.label, state: iv.state,
    })),
    events: page.intervals.map((iv) => eventAt(iv.data)),
}));
`

type crossInterval struct {
	ID     string  `json:"id"`
	LaneID string  `json:"laneId"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
	Label  string  `json:"label"`
	State  string  `json:"state"`
}

type crossEvent struct {
	ID          float64 `json:"id"`
	Kind        string  `json:"kind"`
	Lane        string  `json:"lane"`
	Start       string  `json:"start"`
	DurMs       int64   `json:"dur_ms"`
	Disposition string  `json:"disposition"`
	EventType   string  `json:"event_type"`
	Action      string  `json:"action"`
	DeliveryID  string  `json:"delivery_id"`
	Repo        string  `json:"repo"`
	Method      string  `json:"method"`
	Route       string  `json:"route"`
	Status      int     `json:"status"`
	Actor       string  `json:"actor"`
	ActorName   string  `json:"actor_name"`
	Detail      string  `json:"detail"`
	Target      string  `json:"target"`
	Attempt     int     `json:"attempt"`
	Final       bool    `json:"final"`
}

type crossResult struct {
	MaxID          uint64                        `json:"max_id"`
	RetentionStart float64                       `json:"retention_start"`
	Now            float64                       `json:"now"`
	Lanes          []struct{ Lane, Kind string } `json:"lanes"`
	Intervals      []crossInterval               `json:"intervals"`
	Events         []crossEvent                  `json:"events"`
}

func TestTimelineWireCrossDecode(t *testing.T) {
	module, err := filepath.Abs("web/assets/timeline.js")
	require.NoError(t, err)
	if _, err := os.Stat(module); err != nil {
		// The dashboard JS is a build output (gitignored). Every path that
		// compiles the Go package has already run `npm run build`, so this is
		// only reachable in a hand-run without it.
		t.Skip("run `npm run build` first: " + module + " is missing")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed — cannot cross-check the browser decoder")
	}

	snap := sampleTimeline(t).Snapshot(0)
	payload := filepath.Join(t.TempDir(), "timeline.bin")
	require.NoError(t, os.WriteFile(payload, encodeTimelineV1(snap), 0o644))

	cmd := exec.Command("node", "--input-type=module", "-e",
		fmt.Sprintf(nodeCrossCheck, "file://"+module, payload))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node decode failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("node decode failed: %v", err)
	}

	var got crossResult
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, snap.MaxID, got.MaxID)
	require.Equal(t, float64(snap.RetentionStart.UnixMilli()), got.RetentionStart)
	require.Equal(t, float64(snap.Now.UnixMilli()), got.Now)
	require.Len(t, got.Events, len(snap.Events))
	require.Len(t, got.Intervals, len(snap.Events))

	for i, want := range snap.Events {
		e, iv := got.Events[i], got.Intervals[i]
		startMs := want.Start.UnixMilli()
		require.Equal(t, float64(want.ID), e.ID, "event %d id", i)
		require.Equal(t, want.Kind, e.Kind, "event %d kind", i)
		require.Equal(t, want.Lane, e.Lane, "event %d lane", i)
		require.Equal(t, time.UnixMilli(startMs).UTC().Format("2006-01-02T15:04:05.000Z"), e.Start, "event %d start", i)
		require.Equal(t, want.DurMs, e.DurMs, "event %d dur", i)
		require.Equal(t, want.Disposition, e.Disposition, "event %d disposition", i)
		require.Equal(t, want.EventType, e.EventType, "event %d event_type", i)
		require.Equal(t, want.Action, e.Action, "event %d action", i)
		require.Equal(t, want.DeliveryID, e.DeliveryID, "event %d delivery_id", i)
		require.Equal(t, want.Repo, e.Repo, "event %d repo", i)
		require.Equal(t, want.Method, e.Method, "event %d method", i)
		require.Equal(t, want.Route, e.Route, "event %d route", i)
		require.Equal(t, want.Status, e.Status, "event %d status", i)
		require.Equal(t, want.Actor, e.Actor, "event %d actor", i)
		require.Equal(t, want.ActorName, e.ActorName, "event %d actor_name", i)
		require.Equal(t, want.Detail, e.Detail, "event %d detail", i)
		require.Equal(t, want.Target, e.Target, "event %d target", i)
		require.Equal(t, want.Attempt, e.Attempt, "event %d attempt", i)
		require.Equal(t, want.Final, e.Final, "event %d final", i)

		// The interval is what the chart actually draws: real duration, never
		// an inflated or negative span.
		require.Equal(t, want.Lane, iv.LaneID, "interval %d lane", i)
		require.Equal(t, float64(startMs), iv.Start, "interval %d start", i)
		require.Equal(t, float64(startMs+want.DurMs), iv.End, "interval %d end", i)
	}

	// Lanes must come back with their kinds — that is what orders the chart's
	// rows (webhooks, then requests, then notify).
	require.NotEmpty(t, got.Lanes)
	for _, l := range got.Lanes {
		require.NotEmpty(t, l.Lane)
		require.NotEmpty(t, l.Kind)
	}
}
