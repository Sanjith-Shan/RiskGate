package stream

import (
	"bytes"
	"fmt"

	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// aggregator owns the velocity state of the entity keys that hash to one
// entity-events partition. For every event the merger releases it runs
// Engine.ScoreAndUpdateEntity, the per-entity half of the one feature
// implementation, and sends that entity's features to the joiner.
type aggregator struct {
	part   int32
	parts  int32 // joiner partitions
	engine *features.Engine
	row    schema.Row
	slots  [features.NumEntities][]int
	merge  *merger
	seq    uint64 // events applied, which numbers the outputs
	// arrival applies events as they arrive, deduplicated but with no
	// watermarks: the negative control that shows why the merger exists.
	arrival bool

	// counters, in the snapshot so a restart does not reset them
	applied [features.NumEntities]uint64
}

func newAggregator(cat *schema.Catalog, part, sources, joinParts int32) (*aggregator, error) {
	engine, err := features.NewEngine(cat, features.NewExact(0))
	if err != nil {
		return nil, err
	}
	a := &aggregator{part: part, parts: joinParts, engine: engine, row: engine.NewRow(), merge: newMerger(int(sources))}
	for ent := range features.Entity(features.NumEntities) {
		a.slots[ent] = engine.EntitySlots(ent)
	}
	return a, nil
}

// handle processes one entity-events record and appends the parts it
// releases to out.
func (a *aggregator) handle(value []byte, out []output) ([]output, error) {
	ev, wm, err := decodeAggregatorInput(value)
	if err != nil {
		return out, err
	}
	if ev == nil {
		if err := a.merge.watermark(wm); err != nil {
			return out, err
		}
	} else {
		if a.arrival {
			return a.applyOnArrival(ev, out)
		}
		late, err := a.merge.add(ev)
		if err != nil {
			return out, err
		}
		if late {
			if out, err = a.apply(ev, out); err != nil {
				return out, err
			}
		}
	}
	err = a.merge.release(func(ev *EntityEvent) error {
		out, err = a.apply(ev, out)
		return err
	})
	return out, err
}

func (a *aggregator) applyOnArrival(ev *EntityEvent, out []output) ([]output, error) {
	if err := a.merge.checkSource(ev.Src); err != nil {
		return out, err
	}
	mark := seqMark{ev.Offset, ev.Entity}
	if !mark.after(a.merge.recv[ev.Src]) {
		a.merge.dups++
		return out, nil
	}
	a.merge.recv[ev.Src] = mark
	return a.apply(ev, out)
}

func (a *aggregator) apply(ev *EntityEvent, out []output) ([]output, error) {
	ent := features.Entity(ev.Entity)
	if ent >= features.NumEntities {
		return out, fmt.Errorf("stream: entity %d", ev.Entity)
	}
	if !a.engine.ScoreAndUpdateEntity(&ev.Txn, ent, a.row) {
		return out, fmt.Errorf("stream: payment %s has no %v key, but the router sent one", ev.PaymentID, ent)
	}
	p := EntityPart{Agg: a.part, Seq: a.seq, PaymentID: ev.PaymentID, Entity: ev.Entity, Values: make([]float64, len(a.slots[ent]))}
	for i, s := range a.slots[ent] {
		p.Values[i] = a.row.Num[s]
	}
	a.seq++
	a.applied[ent]++
	return append(out, output{
		topic: topicParts, partition: PaymentPartition(ev.PaymentID, a.parts),
		key: ev.PaymentID, value: appendEntityPart(nil, &p),
	}), nil
}

const aggregatorSnapshotVersion = 1

func (a *aggregator) snapshot() ([]byte, error) {
	e := &enc{}
	e.uvarint(aggregatorSnapshotVersion)
	e.uvarint(a.seq)
	for _, n := range a.applied {
		e.uvarint(n)
	}
	a.merge.encode(e)
	var st bytes.Buffer
	if err := a.engine.State().Snapshot(&st); err != nil {
		return nil, err
	}
	e.bytes(st.Bytes())
	return e.b, nil
}

func (a *aggregator) restore(b []byte) error {
	d := &dec{b: b}
	if v := d.uvarint(); d.err == nil && v != aggregatorSnapshotVersion {
		return fmt.Errorf("stream: aggregator snapshot version %d", v)
	}
	a.seq = d.uvarint()
	for i := range a.applied {
		a.applied[i] = d.uvarint()
	}
	m, err := decodeMerger(d, len(a.merge.wm))
	if err != nil {
		return err
	}
	st := d.raw()
	if err := d.done(); err != nil {
		return err
	}
	a.merge = m
	return a.engine.State().Restore(bytes.NewReader(st))
}
