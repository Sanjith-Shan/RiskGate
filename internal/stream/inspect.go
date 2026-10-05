package stream

import (
	"github.com/Sanjith-Shan/RiskGate/internal/features"
)

// StateSummary adds up what the stateful stages' snapshots say they did.
// Snapshots roll back with the state on a crash, so these counts are exact
// even across crashes: an event applied twice, or never, shows up here.
type StateSummary struct {
	AggregatorPartitions int                          `json:"aggregator_partitions"`
	Applied              [features.NumEntities]uint64 `json:"entity_events_applied"` // by entity: card, uid, device, email
	AppliedTotal         uint64                       `json:"entity_events_applied_total"`
	Buffered             int                          `json:"entity_events_still_buffered"`
	Late                 uint64                       `json:"late_events"`
	MergerDuplicates     uint64                       `json:"duplicate_entity_events_dropped"`
	JoinerPartitions     int                          `json:"joiner_partitions"`
	Decided              uint64                       `json:"payments_decided"`
	Pending              int                          `json:"payments_pending"`
	JoinerDuplicates     uint64                       `json:"duplicate_parts_dropped"`
	PerAggregator        []uint64                     `json:"applied_per_aggregator_partition"`
}

// SummarizeSnapshots reads every snapshot in store for the pipeline's
// topics.
func SummarizeSnapshots(store Store, t Topics, entityParts, joinParts int32) (StateSummary, error) {
	var s StateSummary
	for p := range entityParts {
		_, b, ok, err := store.Get(t.group(StageAggregate), p)
		if err != nil {
			return s, err
		}
		if !ok {
			s.PerAggregator = append(s.PerAggregator, 0)
			continue
		}
		a := &aggregator{merge: newMerger(0)}
		if err := a.restoreCounters(b); err != nil {
			return s, err
		}
		s.AggregatorPartitions++
		var n uint64
		for e, c := range a.applied {
			s.Applied[e] += c
			n += c
		}
		s.AppliedTotal += n
		s.PerAggregator = append(s.PerAggregator, n)
		s.Buffered += len(a.merge.buf)
		s.Late += a.merge.late
		s.MergerDuplicates += a.merge.dups
	}
	for p := range joinParts {
		_, b, ok, err := store.Get(t.group(StageJoin), p)
		if err != nil {
			return s, err
		}
		if !ok {
			continue
		}
		j := &joiner{pending: map[string]*join{}, lastSrc: map[int32]int64{}, lastAgg: map[int32]uint64{}}
		if err := j.restore(b); err != nil {
			return s, err
		}
		s.JoinerPartitions++
		s.Decided += j.decided
		s.Pending += len(j.pending)
		s.JoinerDuplicates += j.dups
	}
	return s, nil
}

// restoreCounters reads an aggregator snapshot's counters and merger,
// without the velocity state.
func (a *aggregator) restoreCounters(b []byte) error {
	d := &dec{b: b}
	d.uvarint() // version
	a.seq = d.uvarint()
	for i := range a.applied {
		a.applied[i] = d.uvarint()
	}
	n := int(peekUvarint(d.b))
	m, err := decodeMerger(d, n)
	if err != nil {
		return err
	}
	a.merge = m
	return nil
}

func peekUvarint(b []byte) uint64 {
	d := dec{b: b}
	return d.uvarint()
}
