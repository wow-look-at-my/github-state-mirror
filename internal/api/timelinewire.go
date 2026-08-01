package api

import (
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/js-snippets/timelinewire"
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
// THE FORMAT IS NOT IMPLEMENTED HERE. Encoder and decoder both live in
// js-snippets alongside <timeline-view>, held together by one golden payload
// in that repo; this file only maps a Snapshot onto the schema below. The
// column NAMES never reach the wire — they are ours, and the library stays
// free of our vocabulary.
//
// Content-negotiated: a client sending Accept: application/vnd.gsm.timeline.v1
// gets this; everything else — curl, the demo preview, anything that does not
// know the format — keeps getting plain JSON. The format is versioned in BOTH
// the media type and the magic bytes, and is never evolved in place: a change
// is v2, so an old client can never mis-read a new payload.

// timelineWireType is the media type that selects the columnar encoding, in
// both the request's Accept and the response's Content-Type.
const timelineWireType = "application/vnd.gsm.timeline.v1"

// timelineSchema names our columns. Order within each group is WIRE ORDER, and
// the chart's SCHEMA constant in web/src/timeline.ts must match it exactly —
// TestTimelineSchemaMatchesChart pins that, because the library never sees a
// column name and a disagreement would surface only in the browser.
var timelineSchema = timelinewire.Schema{
	Magic:  "TLC1",
	DeltaU: []string{"id"},
	DeltaZ: []string{"start"},
	Plain:  []string{"dur", "status", "attempt"},
	Bits:   []string{"final"},
	Strings: []string{
		"kind", "lane", "disposition", "event_type", "action", "delivery_id",
		"repo", "method", "route", "actor", "actor_name", "detail", "target",
	},
}

// encodeTimelineV1 renders a snapshot in the columnar format. It carries the
// exact same information as timelineResponse — events plus the max_id cursor
// and the retention/now boundary — so the two encodings are interchangeable.
func encodeTimelineV1(snap reqtimeline.Snapshot) ([]byte, error) {
	return timelinewire.Encode(timelinePage(snap), timelineSchema)
}

// timelinePage is the whole of our side of the format: a Snapshot laid out as
// columns under the names timelineSchema declares.
func timelinePage(snap reqtimeline.Snapshot) timelinewire.Page {
	ev := snap.Events
	n := len(ev)

	id := make([]uint64, n)
	start := make([]int64, n)
	dur := make([]uint64, n)
	status := make([]uint64, n)
	attempt := make([]uint64, n)
	final := make([]bool, n)
	for i := range ev {
		id[i] = ev[i].ID
		start[i] = ev[i].Start.UnixMilli()
		dur[i] = uint64(ev[i].DurMs)
		status[i] = uint64(ev[i].Status)
		attempt[i] = uint64(ev[i].Attempt)
		final[i] = ev[i].Final
	}

	str := make(map[string][]string, len(timelineSchema.Strings))
	for _, name := range timelineSchema.Strings {
		col := make([]string, n)
		for i := range ev {
			col[i] = stringCol(&ev[i], name)
		}
		str[name] = col
	}

	return timelinewire.Page{
		N:                n,
		MaxID:            snap.MaxID,
		RetentionStartMs: snap.RetentionStart.UnixMilli(),
		NowMs:            snap.Now.UnixMilli(),
		U:                map[string][]uint64{"id": id},
		Z:                map[string][]int64{"start": start},
		P:                map[string][]uint64{"dur": dur, "status": status, "attempt": attempt},
		B:                map[string][]bool{"final": final},
		S:                str,
	}
}

// stringCol returns event e's value for the named string column.
func stringCol(e *reqtimeline.Event, name string) string {
	switch name {
	case "kind":
		return e.Kind
	case "lane":
		return e.Lane
	case "disposition":
		return e.Disposition
	case "event_type":
		return e.EventType
	case "action":
		return e.Action
	case "delivery_id":
		return e.DeliveryID
	case "repo":
		return e.Repo
	case "method":
		return e.Method
	case "route":
		return e.Route
	case "actor":
		return e.Actor
	case "actor_name":
		return e.ActorName
	case "detail":
		return e.Detail
	default:
		return e.Target
	}
}

// wantsTimelineWire reports whether the caller asked for the columnar
// encoding. Deliberately an exact media-type match on Accept: a wildcard
// (*/*, which every browser and curl sends) keeps meaning readable JSON, so
// the endpoint stays inspectable by hand. The chart cannot be harmed by that
// default — it sends only this media type and refuses anything else.
func wantsTimelineWire(accept string) bool {
	for _, part := range splitList(accept) {
		if mediaTypeOf(part) == timelineWireType {
			return true
		}
	}
	return false
}
