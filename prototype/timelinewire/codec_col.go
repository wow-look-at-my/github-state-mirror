package tlwire

import (
	"encoding/binary"
	"fmt"
	"time"
)

// ColCodec is hand-rolled format #2: COLUMNAR. Every field becomes its own
// contiguous run — all ids, then all timestamps, then each string field's
// own small dictionary followed by n indices into it.
//
// Two reasons it beats the row layout on both axes:
//   - Size: a per-column dictionary means the lane column's ~80 distinct
//     values index in ONE byte, and a column of near-identical varints is the
//     ideal input for a general compressor (like values sit adjacent, so gzip
//     matches them at short distances instead of across a 200-byte stride).
//   - Speed: decoding is a tight loop per column with no per-record branching,
//     and in the browser each column lands in a typed array.
//
// Absent strings are index 0 (the reserved empty entry), so there is no
// presence mask to encode at all.
type ColCodec struct{}

func (ColCodec) Name() string { return "columnar" }

// The string columns, in wire order. Accessed by index through a switch
// rather than a []*string: building a pointer slice per event per column is
// 1.3M allocations over a full window, which is what made the first cut of
// this codec the slowest one measured.
const numStringCols = 13

func colOf(e *Event, c int) string {
	switch c {
	case 0:
		return e.Kind
	case 1:
		return e.Lane
	case 2:
		return e.Disposition
	case 3:
		return e.EventType
	case 4:
		return e.Action
	case 5:
		return e.DeliveryID
	case 6:
		return e.Repo
	case 7:
		return e.Method
	case 8:
		return e.Route
	case 9:
		return e.Actor
	case 10:
		return e.ActorName
	case 11:
		return e.Detail
	}
	return e.Target
}

func setCol(e *Event, c int, s string) {
	switch c {
	case 0:
		e.Kind = s
	case 1:
		e.Lane = s
	case 2:
		e.Disposition = s
	case 3:
		e.EventType = s
	case 4:
		e.Action = s
	case 5:
		e.DeliveryID = s
	case 6:
		e.Repo = s
	case 7:
		e.Method = s
	case 8:
		e.Route = s
	case 9:
		e.Actor = s
	case 10:
		e.ActorName = s
	case 11:
		e.Detail = s
	default:
		e.Target = s
	}
}

func (ColCodec) Encode(ev []Event) ([]byte, error) {
	n := len(ev)
	buf := append([]byte{}, "TLC1"...)
	buf = binary.AppendUvarint(buf, uint64(n))

	// Numeric columns.
	var prev uint64
	for i := range ev { // id: delta (almost always 1)
		buf = binary.AppendUvarint(buf, ev[i].ID-prev)
		prev = ev[i].ID
	}
	var prevMs int64
	for i := range ev { // start: signed delta in ms
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
	// final: bitset.
	bits := make([]byte, (n+7)/8)
	for i := range ev {
		if ev[i].Final {
			bits[i/8] |= 1 << (i % 8)
		}
	}
	buf = append(buf, bits...)

	// String columns: per-column dictionary, then n indices.
	for c := 0; c < numStringCols; c++ {
		d := newDict()
		idxs := make([]uint64, n)
		for i := range ev {
			s := colOf(&ev[i], c)
			d.add(s)
			idxs[i] = uint64(d.idx(s))
		}
		buf = d.appendTo(buf)
		// A column nothing in this window used (dictionary = just the
		// reserved empty entry) writes NO index run at all: n varints of zero
		// is 100 KB of nothing, and the decoder infers it from the dict size.
		if len(d.strs) == 1 {
			continue
		}
		for _, ix := range idxs {
			buf = binary.AppendUvarint(buf, ix)
		}
	}
	return buf, nil
}

func (ColCodec) Decode(b []byte) ([]Event, error) {
	if len(b) < 4 || string(b[:4]) != "TLC1" {
		return nil, fmt.Errorf("bad magic")
	}
	r := &reader{b: b[4:]}
	n := int(r.uvarint())
	out := make([]Event, n)

	var prev uint64
	for i := 0; i < n; i++ {
		prev += r.uvarint()
		out[i].ID = prev
	}
	var prevMs int64
	for i := 0; i < n; i++ {
		prevMs += r.varint()
		out[i].Start = time.UnixMilli(prevMs).UTC()
	}
	for i := 0; i < n; i++ {
		out[i].DurMs = int64(r.uvarint())
	}
	for i := 0; i < n; i++ {
		out[i].Status = int(r.uvarint())
	}
	for i := 0; i < n; i++ {
		out[i].Attempt = int(r.uvarint())
	}
	nb := (n + 7) / 8
	if len(r.b) < nb {
		return nil, fmt.Errorf("truncated bitset")
	}
	bits := r.b[:nb]
	r.b = r.b[nb:]
	for i := 0; i < n; i++ {
		out[i].Final = bits[i/8]&(1<<(i%8)) != 0
	}

	for c := 0; c < numStringCols; c++ {
		d := r.dict()
		if r.err != nil {
			return nil, r.err
		}
		if len(d) == 1 { // empty column: no index run on the wire
			continue
		}
		for i := 0; i < n; i++ {
			ix := r.uvarint()
			if int(ix) >= len(d) {
				return nil, fmt.Errorf("dict index out of range")
			}
			setCol(&out[i], c, d[ix])
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return out, nil
}
