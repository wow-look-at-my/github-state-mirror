package tlwire

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// ProtoCodec encodes timeline.proto's `repeated Event` on the wire, hand-built
// with protowire. The bytes are exactly what protoc-gen-go's generated
// Marshal emits for that schema (same field numbers, same proto3
// don't-emit-zero-values rule), so the measurement is a real protobuf
// measurement without dragging protoc into the bench.
type ProtoCodec struct{}

func (ProtoCodec) Name() string { return "protobuf" }

// Field numbers, per timeline.proto.
const (
	fID = iota + 1
	fKind
	fLane
	fStartMs
	fDurMs
	fDisposition
	fEventType
	fAction
	fDeliveryID
	fRepo
	fMethod
	fRoute
	fStatus
	fActor
	fActorName
	fDetail
	fTarget
	fAttempt
	fFinal
)

func (ProtoCodec) Encode(ev []Event) ([]byte, error) {
	var buf, msg []byte
	for i := range ev {
		msg = appendEventProto(msg[:0], &ev[i])
		buf = protowire.AppendTag(buf, 1, protowire.BytesType) // Timeline.events
		buf = protowire.AppendBytes(buf, msg)
	}
	return buf, nil
}

func appendEventProto(b []byte, e *Event) []byte {
	b = putVarint(b, fID, e.ID)
	b = putStr(b, fKind, e.Kind)
	b = putStr(b, fLane, e.Lane)
	b = putVarint(b, fStartMs, uint64(e.Start.UnixMilli()))
	b = putVarint(b, fDurMs, uint64(e.DurMs))
	b = putStr(b, fDisposition, e.Disposition)
	b = putStr(b, fEventType, e.EventType)
	b = putStr(b, fAction, e.Action)
	b = putStr(b, fDeliveryID, e.DeliveryID)
	b = putStr(b, fRepo, e.Repo)
	b = putStr(b, fMethod, e.Method)
	b = putStr(b, fRoute, e.Route)
	b = putVarint(b, fStatus, uint64(e.Status))
	b = putStr(b, fActor, e.Actor)
	b = putStr(b, fActorName, e.ActorName)
	b = putStr(b, fDetail, e.Detail)
	b = putStr(b, fTarget, e.Target)
	b = putVarint(b, fAttempt, uint64(e.Attempt))
	if e.Final {
		b = protowire.AppendTag(b, fFinal, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	return b
}

func putStr(b []byte, num protowire.Number, s string) []byte {
	if s == "" { // proto3: zero values are not emitted
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func putVarint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func (ProtoCodec) Decode(b []byte) ([]Event, error) {
	out := make([]Event, 0, 1024)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("bad tag")
		}
		b = b[n:]
		if num != 1 || typ != protowire.BytesType {
			return nil, fmt.Errorf("unexpected field %d", num)
		}
		msg, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return nil, fmt.Errorf("bad message")
		}
		b = b[n:]
		e, err := decodeEventProto(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func decodeEventProto(b []byte) (Event, error) {
	var e Event
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return e, fmt.Errorf("bad tag")
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return e, fmt.Errorf("bad varint")
			}
			b = b[n:]
			switch num {
			case fID:
				e.ID = v
			case fStartMs:
				e.Start = time.UnixMilli(int64(v)).UTC()
			case fDurMs:
				e.DurMs = int64(v)
			case fStatus:
				e.Status = int(v)
			case fAttempt:
				e.Attempt = int(v)
			case fFinal:
				e.Final = v != 0
			}
		case protowire.BytesType:
			s, n := protowire.ConsumeString(b)
			if n < 0 {
				return e, fmt.Errorf("bad string")
			}
			b = b[n:]
			switch num {
			case fKind:
				e.Kind = s
			case fLane:
				e.Lane = s
			case fDisposition:
				e.Disposition = s
			case fEventType:
				e.EventType = s
			case fAction:
				e.Action = s
			case fDeliveryID:
				e.DeliveryID = s
			case fRepo:
				e.Repo = s
			case fMethod:
				e.Method = s
			case fRoute:
				e.Route = s
			case fActor:
				e.Actor = s
			case fActorName:
				e.ActorName = s
			case fDetail:
				e.Detail = s
			case fTarget:
				e.Target = s
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return e, fmt.Errorf("bad field")
			}
			b = b[n:]
		}
	}
	return e, nil
}
