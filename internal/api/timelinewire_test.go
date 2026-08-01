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

// These tests cover the mapping — a Snapshot onto a Page, and the column names
// the mapping and the chart must agree on. The layout is js-snippets' and is
// tested there, so no bytes are re-specified here.

func mustEncodeTimeline(t *testing.T, snap reqtimeline.Snapshot) []byte {
	t.Helper()
	b, err := encodeTimelineV1(snap)
	require.NoError(t, err)
	return b
}

type decodedTimeline struct {
	events         []reqtimeline.Event
	maxID          uint64
	retentionStart time.Time
	now            time.Time
}

// decodeTimelineV1 reverses timelinePage: the library decodes the layout, this
// puts the columns back into events so handler tests can assert on what
// /api/timeline actually served.
func decodeTimelineV1(t *testing.T, b []byte) decodedTimeline {
	t.Helper()
	p, err := timelinewire.Decode(b, timelineSchema)
	require.NoError(t, err)

	out := decodedTimeline{
		events:         make([]reqtimeline.Event, p.N),
		maxID:          p.MaxID,
		retentionStart: time.UnixMilli(p.RetentionStartMs).UTC(),
		now:            time.UnixMilli(p.NowMs).UTC(),
	}
	for i := range out.events {
		e := &out.events[i]
		e.ID = p.U["id"][i]
		e.Start = time.UnixMilli(p.Z["start"][i]).UTC()
		e.DurMs = int64(p.P["dur"][i])
		e.Status = int(p.P["status"][i])
		e.Attempt = int(p.P["attempt"][i])
		e.Final = p.B["final"][i]
		e.Kind = p.S["kind"][i]
		e.Lane = p.S["lane"][i]
		e.Disposition = p.S["disposition"][i]
		e.EventType = p.S["event_type"][i]
		e.Action = p.S["action"][i]
		e.DeliveryID = p.S["delivery_id"][i]
		e.Repo = p.S["repo"][i]
		e.Method = p.S["method"][i]
		e.Route = p.S["route"][i]
		e.Actor = p.S["actor"][i]
		e.ActorName = p.S["actor_name"][i]
		e.Detail = p.S["detail"][i]
		e.Target = p.S["target"][i]
	}
	return out
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

// The invariant that matters on this side: the columnar payload carries exactly
// what the JSON payload does. A smaller encoding that drops a field is not
// smaller, it is wrong.
func TestTimelinePageCarriesEveryEventField(t *testing.T) {
	snap := sampleTimeline(t).Snapshot(0)
	p := timelinePage(snap)

	require.Equal(t, len(snap.Events), p.N)
	assert.Equal(t, snap.MaxID, p.MaxID)
	assert.Equal(t, snap.RetentionStart.UnixMilli(), p.RetentionStartMs)
	assert.Equal(t, snap.Now.UnixMilli(), p.NowMs)

	for i, e := range snap.Events {
		assert.Equal(t, e.ID, p.U["id"][i])
		assert.Equal(t, e.Start.UnixMilli(), p.Z["start"][i])
		assert.Equal(t, uint64(e.DurMs), p.P["dur"][i])
		assert.Equal(t, uint64(e.Status), p.P["status"][i])
		assert.Equal(t, uint64(e.Attempt), p.P["attempt"][i])
		assert.Equal(t, e.Final, p.B["final"][i])

		assert.Equal(t, e.Kind, p.S["kind"][i])
		assert.Equal(t, e.Lane, p.S["lane"][i])
		assert.Equal(t, e.Disposition, p.S["disposition"][i])
		assert.Equal(t, e.EventType, p.S["event_type"][i])
		assert.Equal(t, e.Action, p.S["action"][i])
		assert.Equal(t, e.DeliveryID, p.S["delivery_id"][i])
		assert.Equal(t, e.Repo, p.S["repo"][i])
		assert.Equal(t, e.Method, p.S["method"][i])
		assert.Equal(t, e.Route, p.S["route"][i])
		assert.Equal(t, e.Actor, p.S["actor"][i])
		assert.Equal(t, e.ActorName, p.S["actor_name"][i])
		assert.Equal(t, e.Detail, p.S["detail"][i])
		assert.Equal(t, e.Target, p.S["target"][i])
	}

	// And back out through the real encoder and decoder, so this covers the
	// payload the chart receives and not just the page behind it.
	got := decodeTimelineV1(t, mustEncodeTimeline(t, snap))
	require.Equal(t, len(snap.Events), len(got.events))
	assert.Equal(t, snap.MaxID, got.maxID)
	for i, want := range snap.Events {
		// Start is ms-resolution on the wire, as it is in JSON once parsed.
		want.Start = want.Start.Truncate(time.Millisecond).UTC()
		require.Equal(t, want, got.events[i])
	}
}

// Unicode lane names (the "⇐ push" / "⇒ notify" prefixes) must survive the
// round trip — a rune-counted dictionary length would corrupt them.
func TestTimelineWireUnicodeLanes(t *testing.T) {
	tl := reqtimeline.New()
	tl.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d1", "o/r", "applied")
	tl.RecordNotify(time.Now(), time.Millisecond, "h.example.com", 200, 1, true, "applied")
	got := decodeTimelineV1(t, mustEncodeTimeline(t, tl.Snapshot(0)))
	require.Equal(t, "⇐ push", got.events[0].Lane)
	require.Equal(t, "⇒ notify", got.events[1].Lane)
}

// An empty ring must encode to a well-formed payload, not a special case the
// client has to guess at.
func TestTimelineWireEmpty(t *testing.T) {
	b := mustEncodeTimeline(t, reqtimeline.New().Snapshot(0))
	require.GreaterOrEqual(t, len(b), 4)
	require.Equal(t, timelineSchema.Magic, string(b[:4]))
}

// The library never sees a column name — producer and consumer each declare
// their own — so a disagreement encodes perfectly and fails in the browser as
// "trailing bytes". Both declarations are in this repo, and nothing but this
// test stops them drifting apart.
func TestTimelineSchemaMatchesChart(t *testing.T) {
	src, err := os.ReadFile("web/src/timeline.ts")
	require.NoError(t, err)

	assert.Equal(t, timelineSchema.Magic, tsField(t, src, "magic"),
		"the chart checks a different magic than the encoder writes")
	assert.Equal(t, timelineSchema.DeltaU, tsList(t, src, "deltaU"))
	assert.Equal(t, timelineSchema.DeltaZ, tsList(t, src, "deltaZ"))
	assert.Equal(t, timelineSchema.Plain, tsList(t, src, "plain"))
	assert.Equal(t, timelineSchema.Bits, tsList(t, src, "bits"))
	assert.Equal(t, timelineSchema.Strings, tsList(t, src, "strings"),
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

// The size claim, asserted rather than quoted. This is the regression guard for
// the whole feature: a change that quietly reverted to per-event strings would
// still round-trip, and only this test would notice.
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
	// Measured ~24 B/event on realistic mixed traffic (docs/timeline-wire-format.md);
	// this uniform corpus is friendlier, so 40 is a loose ceiling that only a
	// real regression trips.
	assert.LessOrEqual(t, perEvent, 40.0,
		"columnar payload is %.1f B/event (%d bytes) — expected well under 40", perEvent, wire)

}
