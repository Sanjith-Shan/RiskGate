package stream

import (
	"encoding/json"
	"fmt"

	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
)

// Payments-topic records are assess request bodies, exactly what POST
// /v1/assess takes. A heartbeat is a record with the header
// riskgate-kind: heartbeat and a JSON body {"dt", "id"}: the producer's
// promise that the partition will carry nothing at or before that position.
// A producer that writes in event-time order can make that promise; a live
// one sends it every second or so, so that a quiet partition does not hold
// back every aggregator's watermark.
const (
	HeaderKind    = "riskgate-kind"
	KindHeartbeat = "heartbeat"
)

// Heartbeat is a heartbeat's body.
type Heartbeat struct {
	DT int64 `json:"dt"`
	ID int64 `json:"id"`
}

// router fans one payments partition out: one entity event per present
// entity key to the aggregator that owns the key, and the payment itself to
// the joiner. It keeps no state that a restart needs, so it commits its
// offsets as soon as its output is acknowledged, and it sends a watermark to
// every aggregator partition after each batch.
type router struct {
	part      int32
	entParts  int32
	joinParts int32
	last      Pos // the latest position seen on this partition
	sent      Pos // the latest watermark sent

	payments, heartbeats, deadLetters, orderViolations uint64
}

func newRouter(part, entParts, joinParts int32) *router {
	return &router{part: part, entParts: entParts, joinParts: joinParts, last: MinPos, sent: MinPos}
}

// in is one input record, stripped of the client library's type.
type in struct {
	offset    int64
	key       []byte
	value     []byte
	heartbeat bool
	timestamp int64 // Unix ms
}

func (r *router) handle(rec in, out []output) ([]output, error) {
	if rec.heartbeat {
		var hb Heartbeat
		if err := json.Unmarshal(rec.value, &hb); err != nil {
			return r.dead(rec, out, fmt.Errorf("heartbeat: %w", err)), nil
		}
		r.heartbeats++
		if p := Pos(hb); r.last.Less(p) {
			r.last = p // a heartbeat may repeat the latest payment's position
		}
		return out, nil
	}
	t, id, created, err := service.DecodePayment(rec.value)
	if err != nil {
		return r.dead(rec, out, err), nil
	}
	r.payments++
	r.advance(PosOf(&t))
	keys, ok := features.KeysOf(&t)
	base := BasePart{Src: r.part, Offset: rec.offset, PaymentID: id, Created: created, Produced: rec.timestamp, Txn: t}
	for ent := range features.Entity(features.NumEntities) {
		if !ok[ent] {
			continue
		}
		base.Mask |= 1 << ent
		ev := EntityEvent{Src: r.part, Offset: rec.offset, Entity: uint8(ent), PaymentID: id, Produced: rec.timestamp, Txn: t}
		out = append(out, output{
			topic: topicEntityEvents, partition: EntityPartition(keys[ent], r.entParts),
			key: keys[ent].String(), value: appendEntityEvent(nil, &ev),
		})
	}
	return append(out, output{
		topic: topicParts, partition: PaymentPartition(id, r.joinParts),
		key: id, value: appendBasePart(nil, &base),
	}), nil
}

// advance moves the partition's position forward. A position that does not
// move forward breaks the contract the watermarks rest on: it is counted,
// and the aggregators will see the payment's events as late.
func (r *router) advance(p Pos) {
	if !r.last.Less(p) {
		r.orderViolations++
		return
	}
	r.last = p
}

func (r *router) dead(rec in, out []output, err error) []output {
	r.deadLetters++
	return append(out, output{topic: topicDeadLetters, partition: -1, key: string(rec.key), value: rec.value, err: err.Error()})
}

// watermarks returns a watermark for every aggregator partition if the
// position moved since the last ones. The runner sends them only after the
// batch's events are acknowledged.
func (r *router) watermarks(out []output) []output {
	if !r.sent.Less(r.last) {
		return out
	}
	r.sent = r.last
	v := appendWatermark(nil, Watermark{Src: r.part, Pos: r.last})
	for p := range r.entParts {
		out = append(out, output{topic: topicEntityEvents, partition: p, key: "", value: v})
	}
	return out
}
