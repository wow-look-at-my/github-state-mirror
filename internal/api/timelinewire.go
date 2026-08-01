package api

import (
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/js-snippets/timelinewire"
)

// The Timeline chart's columnar encoding. NONE of it is implemented here:
// encoder, decoder and the struct-tag mapping all live in js-snippets next to
// <timeline-view>, pinned there by one golden payload. What is ours is the
// vocabulary, and it is declared where the fields are — the `wire:"name,kind"`
// tags on reqtimeline.Event — so this file is the media type and the
// negotiation, nothing else.
//
// A full 24h ring costs 277 B/event as JSON and 23.8 B/event here (7.5 B
// gzipped), and decodes ~5x faster because nothing parses per record.
// see docs/timeline-wire-format.md

// timelineWireType is the media type that selects the columnar encoding, in
// both the request's Accept and the response's Content-Type. The format is
// versioned in BOTH this and the magic below, and is never evolved in place:
// a change is v2, so an old client can never mis-read a new payload.
const timelineWireType = "application/vnd.gsm.timeline.v1"

// timelineWireMagic is the layout version the payload carries.
const timelineWireMagic = "TLC1"

// encodeTimelineV1 renders a snapshot, carrying exactly what timelineResponse
// does: the events, the max_id cursor, and the retention/now boundary.
func encodeTimelineV1(snap reqtimeline.Snapshot) ([]byte, error) {
	return timelinewire.EncodeRows(snap.Events, timelineHeader(snap), timelineWireMagic)
}

func timelineHeader(snap reqtimeline.Snapshot) timelinewire.Header {
	return timelinewire.Header{
		MaxID:            snap.MaxID,
		RetentionStartMs: snap.RetentionStart.UnixMilli(),
		NowMs:            snap.Now.UnixMilli(),
	}
}

// wantsTimelineWire reports whether the caller asked for the columnar
// encoding. The match is exact so that a wildcard Accept — what curl and
// browsers send — still means JSON and the endpoint stays inspectable by hand.
// That default cannot reach the chart: it sends this media type alone and
// refuses any other answer.
func wantsTimelineWire(accept string) bool {
	return timelinewire.Wants(accept, timelineWireType)
}
