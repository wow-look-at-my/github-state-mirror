package api

import (
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/js-snippets/timelinewire"
)

// Covers only the vocabulary (the `wire:` tags) and the chart's agreement with it -- see docs/timeline-wire-format.md.

func mustEncodeTimeline(t *testing.T, snap reqtimeline.Snapshot) []byte {
	t.Helper()
	b, err := encodeTimelineV1(snap)
	require.NoError(t, err)
	return b
}

func decodeTimeline(t *testing.T, b []byte) ([]reqtimeline.Event, timelinewire.Header) {
	t.Helper()
	var events []reqtimeline.Event
	h, err := timelinewire.DecodeRows(b, &events, timelineWireMagic)
	require.NoError(t, err)
	return events, h
}

// sampleTimeline records one of every event shape the ring can hold.
func sampleTimeline(t *testing.T) *reqtimeline.Recorder {
	t.Helper()
	tl := reqtimeline.New()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 40; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		switch i % 4 {
		case 0:
			tl.RecordRequest(at, time.Duration(i)*time.Millisecond, "GET",
				"/repos/{owner}/{repo}/pulls", 200, DispHit, "user:11347985", "PazerOP")
		case 1:
			tl.RecordWebhook(at, 3*time.Millisecond, "push", "", "d-"+fmt.Sprint(i), "o/r-"+fmt.Sprint(i), "applied")
		case 2:
			tl.RecordNotify(at, 120*time.Millisecond, "hooks.example.com", 500, 2, true, "error")
		default:
			// A rejected delivery: fixed lane, clamped detail, no repo.
			tl.RecordWebhookRejected(at, 0, "unverified", "push", "d-"+fmt.Sprint(i))
		}
	}
	return tl
}

// The invariant that matters on this side: every field of an Event survives
// the round trip, so the columnar payload carries exactly what the JSON one
// does. A field that loses its `wire:` tag fails here rather than going
// quietly missing from the chart.
func TestTimelineWireCarriesEveryEventField(t *testing.T) {
	snap := sampleTimeline(t).Snapshot(0)
	events, h := decodeTimeline(t, mustEncodeTimeline(t, snap))

	assert.Equal(t, snap.MaxID, h.MaxID)
	assert.Equal(t, snap.RetentionStart.UnixMilli(), h.RetentionStartMs)
	assert.Equal(t, snap.Now.UnixMilli(), h.NowMs)
	require.Equal(t, len(snap.Events), len(events))

	for i, want := range snap.Events {
		// Start is ms-resolution on the wire, as it is in JSON once parsed.
		want.Start = want.Start.Truncate(time.Millisecond).UTC()
		require.Equal(t, want, events[i])
	}
}

// Unicode lane names (the "⇐ push" / "⇒ notify" prefixes) must survive the
// round trip — a rune-counted dictionary length would corrupt them.
func TestTimelineWireUnicodeLanes(t *testing.T) {
	tl := reqtimeline.New()
	tl.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d1", "o/r", "applied")
	tl.RecordNotify(time.Now(), time.Millisecond, "h.example.com", 200, 1, true, "applied")
	events, _ := decodeTimeline(t, mustEncodeTimeline(t, tl.Snapshot(0)))
	require.Equal(t, "⇐ push", events[0].Lane)
	require.Equal(t, "⇒ notify", events[1].Lane)
}

// An empty ring must encode to a well-formed payload, not a special case the
// client has to guess at.
func TestTimelineWireEmpty(t *testing.T) {
	b := mustEncodeTimeline(t, reqtimeline.New().Snapshot(0))
	require.GreaterOrEqual(t, len(b), 4)
	require.Equal(t, timelineWireMagic, string(b[:4]))
}

// The library never sees a column name -- producer and consumer each declare
// their own -- so a disagreement encodes perfectly and fails in the browser as
// "trailing bytes". Ours are the `wire:` tags on reqtimeline.Event, the
// chart's are its SCHEMA literal, and nothing but this test compares them.
func TestTimelineSchemaMatchesChart(t *testing.T) {
	schema, err := timelinewire.SchemaOf(reqtimeline.Event{}, timelineWireMagic)
	require.NoError(t, err)

	src, err := os.ReadFile("web/src/timeline.ts")
	require.NoError(t, err)

	assert.Equal(t, schema.Magic, tsField(t, src, "magic"),
		"the chart checks a different magic than the encoder writes")
	assert.Equal(t, schema.DeltaU, tsList(t, src, "deltaU"))
	assert.Equal(t, schema.DeltaZ, tsList(t, src, "deltaZ"))
	assert.Equal(t, schema.Plain, tsList(t, src, "plain"))
	assert.Equal(t, schema.Bits, tsList(t, src, "bits"))
	assert.Equal(t, schema.Strings, tsList(t, src, "strings"),
		"the string columns are ORDER-SENSITIVE on the wire")
}

var (
	tsStr  = regexp.MustCompile(`"([^"]*)"`)
	tsCols = `(?s)const SCHEMA: WireSchema = \{.*?\n\};`
)

// tsList reads one array field out of the chart's SCHEMA literal. A regex
// rather than a TypeScript parser: the literal is a flat list of string
// constants, and a field this cannot find fails the test rather than passing
// vacuously.
func tsList(t *testing.T, src []byte, field string) []string {
	t.Helper()
	block := regexp.MustCompile(tsCols).Find(src)
	require.NotNil(t, block, "web/src/timeline.ts has no `const SCHEMA: WireSchema = {...}` literal")
	m := regexp.MustCompile(`(?s)\b` + field + `:\s*\[(.*?)\]`).FindSubmatch(block)
	require.NotNil(t, m, "SCHEMA has no %s field", field)

	out := []string{}
	for _, s := range tsStr.FindAllSubmatch(m[1], -1) {
		out = append(out, string(s[1]))
	}
	return out
}

func tsField(t *testing.T, src []byte, field string) string {
	t.Helper()
	block := regexp.MustCompile(tsCols).Find(src)
	require.NotNil(t, block)
	m := regexp.MustCompile(`\b` + field + `:\s*"([^"]*)"`).FindSubmatch(block)
	require.NotNil(t, m, "SCHEMA has no %s field", field)
	return string(m[1])
}

// Guards the whole feature: a revert to per-event strings would still round-trip, and only this test would notice.
func TestTimelineWireIsMuchSmallerThanJSON(t *testing.T) {
	tl := reqtimeline.New()
	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := 0; i < 20000; i++ {
		tl.RecordRequest(base.Add(time.Duration(i)*time.Second), 12*time.Millisecond, "GET",
			"/repos/{owner}/{repo}/commits/{ref}/check-runs", 200, DispHit,
			"app-installation:40000000", "installation-account")
	}
	snap := tl.Snapshot(0)
	wire := len(mustEncodeTimeline(t, snap))
	perEvent := float64(wire) / float64(len(snap.Events))
	// Measured ~24 B/event on real traffic (docs/timeline-wire-format.md); 40 is a loose ceiling.
	assert.LessOrEqual(t, perEvent, 40.0,
		"columnar payload is %.1f B/event (%d bytes) — expected well under 40", perEvent, wire)

}
