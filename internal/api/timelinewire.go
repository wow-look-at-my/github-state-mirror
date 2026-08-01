package api

import (
	"encoding/binary"

	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// The Timeline chart's BINARY wire format: the same events as the JSON
// payload, laid out COLUMNAR — every field a contiguous run, strings replaced
// by a per-column dictionary plus one small index per event, ids and
// timestamps delta-coded.
//
// Why it exists: the ring holds 24h capped at 100k events, and as JSON that is
// ~283 B/event (~27 MB) — three quarters of it repeated field names and
// RFC3339 timestamps for numbers that delta-code to one byte. Columnar is
// ~23 B/event raw and ~10 B/event gzipped, and it decodes ~5x faster in the
// browser because there is no per-record parsing at all (see
// docs/timeline-wire-format.md for the measurements against BSON, protobuf
// and a row-oriented variant).
//
// Content-negotiated: a client sending Accept: application/vnd.gsm.timeline.v1
// gets this; everything else — curl, the demo preview, anything that does not
// know the format — keeps getting plain JSON. The format is versioned in BOTH
// the media type and the magic bytes, and is never evolved in place: a change
// is v2, so an old client can never mis-read a new payload.
//
// The decoder is internal/api/web/src/timeline.ts (decodeTimelineV1); the
// round-trip test in timelinewire_test.go carries a Go decoder that mirrors it
// field for field, and is the executable spec both sides answer to.

// timelineWireType is the media type that selects the columnar encoding, in
// both the request's Accept and the response's Content-Type.
const timelineWireType = "application/vnd.gsm.timeline.v1"

// timelineWireMagic prefixes every payload: a client that somehow receives the
// wrong bytes fails loudly on the first four rather than mis-decoding.
const timelineWireMagic = "TLC1"

// The string columns, in wire order. Adding one is a format version bump.
const (
	colKind = iota
	colLane
	colDisposition
	colEventType
	colAction
	colDeliveryID
	colRepo
	colMethod
	colRoute
	colActor
	colActorName
	colDetail
	colTarget
	numStringCols
)

// stringCol returns event e's value for string column c.
func stringCol(e *reqtimeline.Event, c int) string {
	switch c {
	case colKind:
		return e.Kind
	case colLane:
		return e.Lane
	case colDisposition:
		return e.Disposition
	case colEventType:
		return e.EventType
	case colAction:
		return e.Action
	case colDeliveryID:
		return e.DeliveryID
	case colRepo:
		return e.Repo
	case colMethod:
		return e.Method
	case colRoute:
		return e.Route
	case colActor:
		return e.Actor
	case colActorName:
		return e.ActorName
	case colDetail:
		return e.Detail
	default:
		return e.Target
	}
}

// encodeTimelineV1 renders a snapshot in the columnar format. It is the exact
// same information as timelineResponse — events plus the max_id cursor and the
// retention/now boundary — so the two encodings are interchangeable.
func encodeTimelineV1(snap reqtimeline.Snapshot) []byte {
	ev := snap.Events
	n := len(ev)

	// Sized for the measured ~24 B/event so the common case never regrows.
	buf := make([]byte, 0, 512+n*24)
	buf = append(buf, timelineWireMagic...)
	buf = binary.AppendUvarint(buf, snap.MaxID)
	buf = binary.AppendVarint(buf, snap.RetentionStart.UnixMilli())
	buf = binary.AppendVarint(buf, snap.Now.UnixMilli())
	buf = binary.AppendUvarint(buf, uint64(n))

	// Numeric columns. ids are consecutive and starts are monotonic in
	// practice, so both delta to one byte; durations/statuses/attempts are
	// small. A signed delta on start keeps the format correct anyway if two
	// events are ever recorded out of start order (they are recorded at
	// COMPLETION, so a long fetch can finish after a short one that started
	// later).
	var prevID uint64
	for i := range ev {
		buf = binary.AppendUvarint(buf, ev[i].ID-prevID)
		prevID = ev[i].ID
	}
	var prevMs int64
	for i := range ev {
		ms := ev[i].Start.UnixMilli()
		buf = binary.AppendVarint(buf, ms-prevMs)
		prevMs = ms
	}
	for i := range ev {
		buf = binary.AppendUvarint(buf, uint64(ev[i].DurMs))
	}
	for i := range ev {
		buf = binary.AppendUvarint(buf, uint64(ev[i].Status))
	}
	for i := range ev {
		buf = binary.AppendUvarint(buf, uint64(ev[i].Attempt))
	}
	bits := make([]byte, (n+7)/8)
	for i := range ev {
		if ev[i].Final {
			bits[i/8] |= 1 << (i % 8)
		}
	}
	buf = append(buf, bits...)

	// String columns: dictionary, then one index per event. Index 0 is the
	// reserved empty string, so an absent field needs no presence bit — and a
	// column no event in this window used (detail, target, ... on a
	// request-only window) is written as a dictionary of one and NO index run
	// at all, which the decoder infers from the dictionary size.
	idxs := make([]uint64, n)
	for c := 0; c < numStringCols; c++ {
		d := newWireDict()
		for i := range ev {
			idxs[i] = uint64(d.index(stringCol(&ev[i], c)))
		}
		buf = d.appendTo(buf)
		if len(d.strs) == 1 {
			continue
		}
		for _, ix := range idxs {
			buf = binary.AppendUvarint(buf, ix)
		}
	}
	return buf
}

// wireDict interns one column's distinct strings, entry 0 always "".
type wireDict struct {
	byStr map[string]int
	strs  []string
}

func newWireDict() *wireDict {
	return &wireDict{byStr: map[string]int{"": 0}, strs: []string{""}}
}

func (d *wireDict) index(s string) int {
	if i, ok := d.byStr[s]; ok {
		return i
	}
	i := len(d.strs)
	d.byStr[s] = i
	d.strs = append(d.strs, s)
	return i
}

func (d *wireDict) appendTo(b []byte) []byte {
	b = binary.AppendUvarint(b, uint64(len(d.strs)))
	for _, s := range d.strs {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}

// wantsTimelineWire reports whether the caller asked for the columnar
// encoding. Deliberately an exact media-type match on Accept: a wildcard
// (*/*, which every browser and curl sends) must keep meaning JSON.
func wantsTimelineWire(accept string) bool {
	for _, part := range splitList(accept) {
		if mediaTypeOf(part) == timelineWireType {
			return true
		}
	}
	return false
}
