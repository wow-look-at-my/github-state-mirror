package api

import (
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/js-snippets/timelinewire"
)

// The Timeline chart's columnar encoding. This file is only the media type and negotiation.

// timelineWireType selects the columnar encoding; a layout change is v2, never evolved in place.
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

// wantsTimelineWire matches the Accept EXACTLY, so a wildcard (curl, browsers) still means JSON.
func wantsTimelineWire(accept string) bool {
	return timelinewire.Wants(accept, timelineWireType)
}
