package tlwire

import (
	"encoding/binary"
	"fmt"
	"time"
)

// RowCodec is hand-rolled format #1: still one record per event (a shape a
// streaming/append writer could produce), but with the two things JSON cannot
// do — a shared string dictionary, and numbers as deltas + varints instead of
// decimal text. IDs and timestamps are delta-coded against the previous event,
// which is where most of the win is: the ring is ID-ordered and roughly
// time-ordered, so both deltas are tiny.
type RowCodec struct{}

func (RowCodec) Name() string { return "row" }

// Optional-field mask bits (kind/lane/id/start/dur are unconditional).
const (
	bitDisposition = 1 << iota
	bitEventType
	bitAction
	bitDeliveryID
	bitRepo
	bitMethod
	bitRoute
	bitStatus
	bitActor
	bitActorName
	bitDetail
	bitTarget
	bitAttempt
	bitFinal
)

func (RowCodec) Encode(ev []Event) ([]byte, error) {
	d := newDict()
	for i := range ev {
		e := &ev[i]
		for _, s := range []string{e.Kind, e.Lane, e.Disposition, e.EventType, e.Action, e.DeliveryID, e.Repo, e.Method, e.Route, e.Actor, e.ActorName, e.Detail, e.Target} {
			d.add(s)
		}
	}

	buf := append([]byte{}, "TLR1"...)
	buf = binary.AppendUvarint(buf, uint64(len(ev)))
	buf = d.appendTo(buf)

	var prevID uint64
	var prevMs int64
	for i := range ev {
		e := &ev[i]
		ms := e.Start.UnixMilli()
		buf = binary.AppendUvarint(buf, e.ID-prevID)
		buf = binary.AppendVarint(buf, ms-prevMs)
		buf = binary.AppendUvarint(buf, uint64(e.DurMs))
		prevID, prevMs = e.ID, ms

		var mask uint32
		set := func(bit uint32, cond bool) {
			if cond {
				mask |= bit
			}
		}
		set(bitDisposition, e.Disposition != "")
		set(bitEventType, e.EventType != "")
		set(bitAction, e.Action != "")
		set(bitDeliveryID, e.DeliveryID != "")
		set(bitRepo, e.Repo != "")
		set(bitMethod, e.Method != "")
		set(bitRoute, e.Route != "")
		set(bitStatus, e.Status != 0)
		set(bitActor, e.Actor != "")
		set(bitActorName, e.ActorName != "")
		set(bitDetail, e.Detail != "")
		set(bitTarget, e.Target != "")
		set(bitAttempt, e.Attempt != 0)
		set(bitFinal, e.Final)
		buf = binary.AppendUvarint(buf, uint64(mask))

		buf = binary.AppendUvarint(buf, uint64(d.idx(e.Kind)))
		buf = binary.AppendUvarint(buf, uint64(d.idx(e.Lane)))
		for _, f := range []struct {
			bit uint32
			s   string
		}{
			{bitDisposition, e.Disposition}, {bitEventType, e.EventType}, {bitAction, e.Action},
			{bitDeliveryID, e.DeliveryID}, {bitRepo, e.Repo}, {bitMethod, e.Method},
			{bitRoute, e.Route}, {bitActor, e.Actor}, {bitActorName, e.ActorName},
			{bitDetail, e.Detail}, {bitTarget, e.Target},
		} {
			if mask&f.bit != 0 {
				buf = binary.AppendUvarint(buf, uint64(d.idx(f.s)))
			}
		}
		if mask&bitStatus != 0 {
			buf = binary.AppendUvarint(buf, uint64(e.Status))
		}
		if mask&bitAttempt != 0 {
			buf = binary.AppendUvarint(buf, uint64(e.Attempt))
		}
	}
	return buf, nil
}

func (RowCodec) Decode(b []byte) ([]Event, error) {
	if len(b) < 4 || string(b[:4]) != "TLR1" {
		return nil, fmt.Errorf("bad magic")
	}
	r := &reader{b: b[4:]}
	n := int(r.uvarint())
	dict := r.dict()
	out := make([]Event, 0, n)
	var prevID uint64
	var prevMs int64
	for i := 0; i < n; i++ {
		var e Event
		prevID += r.uvarint()
		prevMs += r.varint()
		e.ID, e.Start, e.DurMs = prevID, time.UnixMilli(prevMs).UTC(), int64(r.uvarint())
		mask := uint32(r.uvarint())
		e.Kind = dict[r.uvarint()]
		e.Lane = dict[r.uvarint()]
		get := func(bit uint32, dst *string) {
			if mask&bit != 0 {
				*dst = dict[r.uvarint()]
			}
		}
		get(bitDisposition, &e.Disposition)
		get(bitEventType, &e.EventType)
		get(bitAction, &e.Action)
		get(bitDeliveryID, &e.DeliveryID)
		get(bitRepo, &e.Repo)
		get(bitMethod, &e.Method)
		get(bitRoute, &e.Route)
		get(bitActor, &e.Actor)
		get(bitActorName, &e.ActorName)
		get(bitDetail, &e.Detail)
		get(bitTarget, &e.Target)
		if mask&bitStatus != 0 {
			e.Status = int(r.uvarint())
		}
		if mask&bitAttempt != 0 {
			e.Attempt = int(r.uvarint())
		}
		e.Final = mask&bitFinal != 0
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, e)
	}
	return out, nil
}

// ---- shared dictionary + reader -----------------------------------------

type dict struct {
	idxByStr map[string]int
	strs     []string
}

func newDict() *dict {
	d := &dict{idxByStr: map[string]int{"": 0}, strs: []string{""}}
	return d
}

func (d *dict) add(s string) {
	if _, ok := d.idxByStr[s]; !ok {
		d.idxByStr[s] = len(d.strs)
		d.strs = append(d.strs, s)
	}
}

func (d *dict) idx(s string) int { return d.idxByStr[s] }

func (d *dict) appendTo(b []byte) []byte {
	b = binary.AppendUvarint(b, uint64(len(d.strs)))
	for _, s := range d.strs {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("truncated uvarint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) varint() int64 {
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("truncated varint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) str() string {
	n := int(r.uvarint())
	if r.err != nil || n > len(r.b) {
		r.err = fmt.Errorf("truncated string")
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func (r *reader) dict() []string {
	n := int(r.uvarint())
	out := make([]string, 0, n)
	for i := 0; i < n && r.err == nil; i++ {
		out = append(out, r.str())
	}
	return out
}
