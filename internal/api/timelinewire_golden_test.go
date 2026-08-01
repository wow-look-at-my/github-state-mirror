package api

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// THE CROSS-LANGUAGE PIN.
//
// The encoder is Go (timelinewire.go); the decoder is TypeScript and lives in
// js-snippets (src/ui/timeline-wire.ts) next to the chart it feeds. Two
// implementations of one layout in two repos need something holding them
// together, and it cannot be "run one from the other" — that was the old
// arrangement, and it made a Go unit test depend on a built browser bundle.
//
// So the contract is BYTES. This test pins the exact payload the encoder
// produces for a fixed snapshot; js-snippets' timeline-wire.test.ts decodes
// the same base64 and asserts the same values. Neither side can drift without
// one of them going red, and neither has to run the other's toolchain.
//
// Changing the layout means a NEW VERSION (new magic, new media type) — so if
// this test fails, either the encoder regressed or you are writing v2, and v2
// gets its own fixture rather than an edit to this one.
const goldenTimelineV1B64 = "VExDMQOAqN2A92egtJHT92cDAQEBgJiQ0/dnuBeIJwMM+gEAyAH2AwAAAAADAAd3ZWJob29rB3JlcXVlc3QBAgIEAAjih5Ag" +
	"cHVzaB9HRVQgL3JlcG9zL3tvd25lcn0ve3JlcG99L3B1bGxzDVBPU1QgL2dyYXBocWwBAgMEAAdhcHBsaWVkA2hpdAVlcnJv" +
	"cgECAwIABHB1c2gBAAABAAIADGQtw5xuaWNvZGUtMQEAAAIACm93bmVyL3JlcG8BAAADAANHRVQEUE9TVAABAgMAGy9yZXBv" +
	"cy97b3duZXJ9L3tyZXBvfS9wdWxscwgvZ3JhcGhxbAABAgMABnVzZXI6MQZhcHA6OTkAAQICAAdQYXplck9QAAEAAQABAA=="

// goldenSnapshot is the fixed input behind goldenTimelineV1B64: three events
// covering what the layout has to survive — a webhook and two requests (so the
// kind/lane/disposition dictionaries hold more than one entry), a non-ASCII
// delivery id, an actor name on only one row, and columns (target, detail,
// attempt, final) that no row uses, which the encoder writes as a
// single-entry dictionary with no index run.
//
// Times are literals, never time.Now(): the payload carries retention_start
// and now, so a clock in here would make the bytes unpinnable.
func goldenSnapshot() reqtimeline.Snapshot {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return reqtimeline.Snapshot{
		MaxID:          3,
		RetentionStart: base.Add(-24 * time.Hour),
		Now:            base.Add(10 * time.Second),
		Events: []reqtimeline.Event{
			{
				ID: 1, Kind: reqtimeline.KindWebhook, Lane: "⇐ push",
				Start: base, DurMs: 3, Disposition: "applied",
				EventType: "push", DeliveryID: "d-Ünicode-1", Repo: "owner/repo",
			},
			{
				ID: 2, Kind: reqtimeline.KindRequest, Lane: "GET /repos/{owner}/{repo}/pulls",
				Start: base.Add(1500 * time.Millisecond), DurMs: 12, Disposition: DispHit,
				Method: "GET", Route: "/repos/{owner}/{repo}/pulls", Status: 200,
				Actor: "user:1", ActorName: "PazerOP",
			},
			{
				ID: 3, Kind: reqtimeline.KindRequest, Lane: "POST /graphql",
				Start: base.Add(4 * time.Second), DurMs: 250, Disposition: DispError,
				Method: "POST", Route: "/graphql", Status: 502, Actor: "app:99",
			},
		},
	}
}

func TestTimelineWireGoldenBytes(t *testing.T) {
	got := base64.StdEncoding.EncodeToString(encodeTimelineV1(goldenSnapshot()))
	require.Equal(t, goldenTimelineV1B64, got,
		"the v1 payload changed.\n\nThis layout is shared with the TypeScript "+
			"decoder in js-snippets (src/ui/timeline-wire.ts), which pins these "+
			"same bytes in timeline-wire.test.ts. If you meant to change the "+
			"format, that is a NEW VERSION — new magic, new media type, new "+
			"fixture — not an edit to this one.\n\ngot: %s", got)
}

// TestTimelineWireGoldenDecodes keeps the fixture honest from this side too:
// the bytes must still decode, through the Go mirror decoder, to the snapshot
// they were built from. A fixture nobody decodes is just a string.
func TestTimelineWireGoldenDecodes(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(goldenTimelineV1B64)
	require.NoError(t, err)

	got := decodeTimelineV1(t, raw)
	want := goldenSnapshot()
	require.Equal(t, want.MaxID, got.maxID)
	require.Len(t, got.events, len(want.Events))
	for i, w := range want.Events {
		require.Equal(t, w.Lane, got.events[i].Lane, "event %d lane", i)
		require.Equal(t, w.Kind, got.events[i].Kind, "event %d kind", i)
		require.Equal(t, w.DeliveryID, got.events[i].DeliveryID, "event %d delivery id", i)
		require.Equal(t, w.Status, got.events[i].Status, "event %d status", i)
		require.Equal(t, w.ActorName, got.events[i].ActorName, "event %d actor name", i)
	}
}
