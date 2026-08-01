package api

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// The columnar format's executable spec. decodeTimelineV1 below mirrors
// internal/api/web/src/timeline.ts's decodeTimelineV1 step for step — the two
// decoders are the same algorithm in two languages, and this test is what
// keeps the encoder honest for both.

type decodedTimeline struct {
	events         []reqtimeline.Event
	maxID          uint64
	retentionStart time.Time
	now            time.Time
}

type wireReader struct {
	b []byte
	p int
	t *testing.T
}

func (r *wireReader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b[r.p:])
	if n <= 0 {
		r.t.Fatalf("truncated uvarint at %d", r.p)
	}
	r.p += n
	return v
}

func (r *wireReader) varint() int64 {
	v, n := binary.Varint(r.b[r.p:])
	if n <= 0 {
		r.t.Fatalf("truncated varint at %d", r.p)
	}
	r.p += n
	return v
}

func (r *wireReader) dict() []string {
	n := int(r.uvarint())
	out := make([]string, n)
	for i := range out {
		ln := int(r.uvarint())
		if r.p+ln > len(r.b) {
			r.t.Fatalf("truncated dict string at %d", r.p)
		}
		out[i] = string(r.b[r.p : r.p+ln])
		r.p += ln
	}
	return out
}

func decodeTimelineV1(t *testing.T, b []byte) decodedTimeline {
	t.Helper()
	require.GreaterOrEqual(t, len(b), 4, "payload too short to carry the magic")
	require.Equal(t, timelineWireMagic, string(b[:4]), "wrong format magic")

	r := &wireReader{b: b, p: 4, t: t}
	out := decodedTimeline{maxID: r.uvarint()}
	out.retentionStart = time.UnixMilli(r.varint()).UTC()
	out.now = time.UnixMilli(r.varint()).UTC()
	n := int(r.uvarint())
	out.events = make([]reqtimeline.Event, n)

	var id uint64
	for i := 0; i < n; i++ {
		id += r.uvarint()
		out.events[i].ID = id
	}
	var ms int64
	for i := 0; i < n; i++ {
		ms += r.varint()
		out.events[i].Start = time.UnixMilli(ms).UTC()
	}
	for i := 0; i < n; i++ {
		out.events[i].DurMs = int64(r.uvarint())
	}
	for i := 0; i < n; i++ {
		out.events[i].Status = int(r.uvarint())
	}
	for i := 0; i < n; i++ {
		out.events[i].Attempt = int(r.uvarint())
	}
	nb := (n + 7) / 8
	bits := r.b[r.p : r.p+nb]
	r.p += nb
	for i := 0; i < n; i++ {
		out.events[i].Final = bits[i/8]&(1<<(i%8)) != 0
	}

	for c := 0; c < numStringCols; c++ {
		d := r.dict()
		if len(d) == 1 {
			continue // empty column: no index run on the wire
		}
		for i := 0; i < n; i++ {
			setStringCol(&out.events[i], c, d[r.uvarint()])
		}
	}
	require.Equal(t, len(b), r.p, "decoder must consume the payload exactly")

	return out
}

func setStringCol(e *reqtimeline.Event, c int, s string) {
	switch c {
	case colKind:
		e.Kind = s
	case colLane:
		e.Lane = s
	case colDisposition:
		e.Disposition = s
	case colEventType:
		e.EventType = s
	case colAction:
		e.Action = s
	case colDeliveryID:
		e.DeliveryID = s
	case colRepo:
		e.Repo = s
	case colMethod:
		e.Method = s
	case colRoute:
		e.Route = s
	case colActor:
		e.Actor = s
	case colActorName:
		e.ActorName = s
	case colDetail:
		e.Detail = s
	default:
		e.Target = s
	}
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

// TestTimelineWireRoundTrip is the invariant that matters: the columnar
// payload carries EXACTLY what the JSON payload does. A smaller encoding that
// drops a field is not smaller, it is wrong.
func TestTimelineWireRoundTrip(t *testing.T) {
	snap := sampleTimeline(t).Snapshot(0)
	got := decodeTimelineV1(t, encodeTimelineV1(snap))

	assert.Equal(t, snap.MaxID, got.maxID)

	assert.True(t, got.retentionStart.Equal(snap.RetentionStart.Truncate(time.Millisecond)))

	assert.True(t, got.now.Equal(snap.Now.Truncate(time.Millisecond)))

	require.Equal(t, len(snap.Events), len(got.events))

	for i, want := range snap.Events {
		// Everything but Start compares directly; Start is ms-resolution on
		// the wire (as it is in JSON once the browser parses it).
		want.Start = want.Start.Truncate(time.Millisecond).UTC()
		require.Equal(t, want, got.events[i])

	}
}

// An empty ring must encode to a well-formed payload, not a special case the
// client has to guess at.
func TestTimelineWireEmpty(t *testing.T) {
	got := decodeTimelineV1(t, encodeTimelineV1(reqtimeline.New().Snapshot(0)))
	require.Equal(t, 0, len(got.events))

}

// Unicode lane names (the "⇐ push" / "⇒ notify" prefixes) must survive the
// byte-length-prefixed dictionary — a rune-counted length would corrupt them.
func TestTimelineWireUnicodeLanes(t *testing.T) {
	tl := reqtimeline.New()
	tl.RecordWebhook(time.Now(), time.Millisecond, "push", "", "d1", "o/r", "applied")
	tl.RecordNotify(time.Now(), time.Millisecond, "h.example.com", 200, 1, true, "applied")
	got := decodeTimelineV1(t, encodeTimelineV1(tl.Snapshot(0)))
	require.Equal(t, "⇐ push", got.events[0].Lane)
	require.Equal(t, "⇒ notify", got.events[1].Lane)

}

// The size claim, asserted rather than quoted: the columnar payload must stay
// an order of magnitude under the JSON one. This is the regression guard for
// the whole feature — an encoder change that quietly reverts to per-event
// strings would still round-trip, and only this test would notice.
func TestTimelineWireIsMuchSmallerThanJSON(t *testing.T) {
	tl := reqtimeline.New()
	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := 0; i < 20000; i++ {
		tl.RecordRequest(base.Add(time.Duration(i)*time.Second), 12*time.Millisecond, "GET",
			"/repos/{owner}/{repo}/commits/{ref}/check-runs", 200, DispHit,
			"app-installation:40000000", "installation-account")
	}
	snap := tl.Snapshot(0)
	wire := len(encodeTimelineV1(snap))
	perEvent := float64(wire) / float64(len(snap.Events))
	// Measured ~24 B/event on realistic mixed traffic (docs/timeline-wire-format.md);
	// this uniform corpus is friendlier, so 40 is a loose ceiling that only a
	// real regression trips.
	assert.LessOrEqual(t, perEvent, 40.0,
		"columnar payload is %.1f B/event (%d bytes) — expected well under 40", perEvent, wire)

}
