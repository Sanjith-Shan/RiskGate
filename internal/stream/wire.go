package stream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

// Wire formats of the internal topics. Payments, decisions and disputes are
// JSON, because other systems write and read them. The two topics only
// RiskGate reads (entity events and parts) are a small binary format:
// floats travel as their IEEE-754 bits, so a NaN, a signed zero or the last
// bit of a dollar amount cannot change on the way, which the 0-mismatch
// parity result depends on.

// Pos is a payment's place in event time: TransactionDT, ties broken by
// TransactionID. It is the same total order data.Less defines and Replay
// requires.
type Pos struct {
	DT, ID int64
}

// PosOf returns t's position.
func PosOf(t *data.Txn) Pos { return Pos{t.DT, t.ID} }

// Less orders positions like data.Less orders payments.
func (p Pos) Less(q Pos) bool {
	if p.DT != q.DT {
		return p.DT < q.DT
	}
	return p.ID < q.ID
}

// MinPos is before every payment; MaxPos is after every payment, the
// watermark of a source that has finished.
var (
	MinPos = Pos{math.MinInt64, math.MinInt64}
	MaxPos = Pos{math.MaxInt64, math.MaxInt64}
)

// Record kinds, the first byte of every internal record.
const (
	kindEntityEvent byte = 1 // router -> aggregator: one entity of one payment
	kindWatermark   byte = 2 // router -> aggregator: a source partition's progress
	kindBasePart    byte = 3 // router -> joiner: the payment itself
	kindEntityPart  byte = 4 // aggregator -> joiner: one entity's features
)

var errShort = errors.New("stream: truncated record")

// enc appends values to a byte slice.
type enc struct{ b []byte }

func (e *enc) byte(v byte)      { e.b = append(e.b, v) }
func (e *enc) uvarint(v uint64) { e.b = binary.AppendUvarint(e.b, v) }
func (e *enc) varint(v int64)   { e.b = binary.AppendVarint(e.b, v) }
func (e *enc) f64(v float64)    { e.b = binary.LittleEndian.AppendUint64(e.b, math.Float64bits(v)) }
func (e *enc) str(s string)     { e.uvarint(uint64(len(s))); e.b = append(e.b, s...) }
func (e *enc) bytes(b []byte)   { e.uvarint(uint64(len(b))); e.b = append(e.b, b...) }
func (e *enc) pos(p Pos)        { e.varint(p.DT); e.varint(p.ID) }

// dec reads what enc wrote, with a sticky error.
type dec struct {
	b   []byte
	err error
}

func (d *dec) fail() {
	if d.err == nil {
		d.err = errShort
	}
}

func (d *dec) byte() byte {
	if len(d.b) < 1 {
		d.fail()
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *dec) uvarint() uint64 {
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) varint() int64 {
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) f64() float64 {
	if len(d.b) < 8 {
		d.fail()
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(d.b))
	d.b = d.b[8:]
	return v
}

func (d *dec) raw() []byte {
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b)) {
		d.fail()
		return nil
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

func (d *dec) str() string { return string(d.raw()) }
func (d *dec) pos() Pos    { return Pos{d.varint(), d.varint()} }

// done reports the sticky error, or an error if bytes are left over.
func (d *dec) done() error {
	if d.err == nil && len(d.b) != 0 {
		return fmt.Errorf("stream: %d trailing bytes", len(d.b))
	}
	return d.err
}

// txn encodes every field of a payment.
func (e *enc) txn(t *data.Txn) {
	e.varint(t.ID)
	e.varint(t.DT)
	e.f64(t.Amount)
	e.str(t.ProductCode)
	e.f64(t.Card1)
	e.str(t.Card4)
	e.str(t.Card6)
	e.f64(t.Addr1)
	e.f64(t.Addr2)
	e.f64(t.Dist1)
	e.str(t.PEmail)
	e.str(t.REmail)
	e.str(t.DeviceType)
	e.str(t.DeviceInfo)
	e.f64(t.D1)
	e.byte(byte(t.IsFraud))
}

func (d *dec) txn() data.Txn {
	var t data.Txn
	t.ID = d.varint()
	t.DT = d.varint()
	t.Amount = d.f64()
	t.ProductCode = d.str()
	t.Card1 = d.f64()
	t.Card4 = d.str()
	t.Card6 = d.str()
	t.Addr1 = d.f64()
	t.Addr2 = d.f64()
	t.Dist1 = d.f64()
	t.PEmail = d.str()
	t.REmail = d.str()
	t.DeviceType = d.str()
	t.DeviceInfo = d.str()
	t.D1 = d.f64()
	t.IsFraud = int8(d.byte())
	return t
}

// EntityEvent is one entity of one payment, sent to the aggregator that owns
// the entity's key. Src and Offset name the payments record it came from;
// with Entity they identify it for deduplication.
type EntityEvent struct {
	Src       int32 // payments partition
	Offset    int64 // payments offset
	Entity    uint8
	PaymentID string
	Produced  int64 // payments record timestamp, Unix ms, for latency
	Txn       data.Txn
}

// Watermark says that a payments partition will send nothing at or before
// Pos again, except duplicates of what it already sent.
type Watermark struct {
	Src int32
	Pos Pos
}

// BasePart is the payment itself, sent by the router to the joiner. Mask
// has bit e set for every entity whose features will follow.
type BasePart struct {
	Src       int32
	Offset    int64
	PaymentID string
	Created   int64
	Produced  int64
	Mask      uint8
	Txn       data.Txn
}

// EntityPart is one entity's velocity features for one payment, from the
// aggregator partition Agg, which numbers its outputs with Seq.
type EntityPart struct {
	Agg       int32
	Seq       uint64
	PaymentID string
	Entity    uint8
	Values    []float64 // in features.Engine.EntitySlots order
}

func appendEntityEvent(b []byte, ev *EntityEvent) []byte {
	e := enc{b}
	e.byte(kindEntityEvent)
	e.varint(int64(ev.Src))
	e.varint(ev.Offset)
	e.byte(ev.Entity)
	e.str(ev.PaymentID)
	e.varint(ev.Produced)
	e.txn(&ev.Txn)
	return e.b
}

func appendWatermark(b []byte, w Watermark) []byte {
	e := enc{b}
	e.byte(kindWatermark)
	e.varint(int64(w.Src))
	e.pos(w.Pos)
	return e.b
}

func appendBasePart(b []byte, p *BasePart) []byte {
	e := enc{b}
	e.byte(kindBasePart)
	e.varint(int64(p.Src))
	e.varint(p.Offset)
	e.str(p.PaymentID)
	e.varint(p.Created)
	e.varint(p.Produced)
	e.byte(p.Mask)
	e.txn(&p.Txn)
	return e.b
}

func appendEntityPart(b []byte, p *EntityPart) []byte {
	e := enc{b}
	e.byte(kindEntityPart)
	e.varint(int64(p.Agg))
	e.uvarint(p.Seq)
	e.str(p.PaymentID)
	e.byte(p.Entity)
	e.uvarint(uint64(len(p.Values)))
	for _, v := range p.Values {
		e.f64(v)
	}
	return e.b
}

// decodeAggregatorInput decodes an entity-events record: an *EntityEvent or
// a Watermark.
func decodeAggregatorInput(b []byte) (ev *EntityEvent, wm Watermark, err error) {
	d := dec{b: b}
	switch k := d.byte(); k {
	case kindEntityEvent:
		ev = &EntityEvent{}
		ev.Src = int32(d.varint())
		ev.Offset = d.varint()
		ev.Entity = d.byte()
		ev.PaymentID = d.str()
		ev.Produced = d.varint()
		ev.Txn = d.txn()
	case kindWatermark:
		wm.Src = int32(d.varint())
		wm.Pos = d.pos()
	default:
		if d.err == nil {
			return nil, wm, fmt.Errorf("stream: entity-events record of kind %d", k)
		}
	}
	return ev, wm, d.done()
}

// decodePart decodes a parts record: a *BasePart or an *EntityPart.
func decodePart(b []byte) (*BasePart, *EntityPart, error) {
	d := dec{b: b}
	switch k := d.byte(); k {
	case kindBasePart:
		p := &BasePart{}
		p.Src = int32(d.varint())
		p.Offset = d.varint()
		p.PaymentID = d.str()
		p.Created = d.varint()
		p.Produced = d.varint()
		p.Mask = d.byte()
		p.Txn = d.txn()
		return p, nil, d.done()
	case kindEntityPart:
		p := &EntityPart{}
		p.Agg = int32(d.varint())
		p.Seq = d.uvarint()
		p.PaymentID = d.str()
		p.Entity = d.byte()
		n := d.uvarint()
		if n > 64 {
			return nil, nil, fmt.Errorf("stream: entity part with %d values", n)
		}
		p.Values = make([]float64, n)
		for i := range p.Values {
			p.Values[i] = d.f64()
		}
		return nil, p, d.done()
	default:
		if d.err != nil {
			return nil, nil, d.err
		}
		return nil, nil, fmt.Errorf("stream: parts record of kind %d", k)
	}
}
