package api

import (
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/js-snippets/timelinewire"
)

// Our half of the Timeline chart's columnar wire format: a Snapshot mapped
// onto the column names below. THE FORMAT ITSELF IS NOT HERE — encoder and
// decoder both live in js-snippets next to <timeline-view>, pinned there by
// one golden payload. Column names never reach the wire, which is why the
// library can stay free of our vocabulary.
//
// A full 24h ring costs 277 B/event as JSON and 23.8 B/event here (7.5 B
// gzipped), and decodes ~5x faster because nothing parses per record.
// see docs/timeline-wire-format.md

// timelineWireType is the media type that selects the columnar encoding, in
// both the request's Accept and the response's Content-Type.
const timelineWireType = "application/vnd.gsm.timeline.v1"

// timelineSchema names our columns; order within each group is WIRE ORDER. The
// chart's SCHEMA literal in web/src/timeline.ts must match it exactly, and only
// TestTimelineSchemaMatchesChart says so — a mismatch encodes fine and fails in
// the browser as "trailing bytes".
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

// encodeTimelineV1 renders a snapshot in the columnar format, carrying exactly
// what timelineResponse does: the events, the max_id cursor, and the
// retention/now boundary.
func encodeTimelineV1(snap reqtimeline.Snapshot) ([]byte, error) {
	return timelinewire.Encode(timelinePage(snap), timelineSchema)
}

// timelinePage lays a Snapshot out as columns under timelineSchema's names.
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

// wantsTimelineWire reports whether the caller asked for the columnar encoding.
// The match is exact so that a wildcard Accept — what curl and browsers send —
// still means JSON and the endpoint stays inspectable by hand. That default
// cannot reach the chart: it sends this media type alone and refuses any other
// answer.
func wantsTimelineWire(accept string) bool {
	for _, part := range splitList(accept) {
		if mediaTypeOf(part) == timelineWireType {
			return true
		}
	}
	return false
}
