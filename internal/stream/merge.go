package stream

import (
	"container/heap"
	"fmt"
	"slices"
)

// merger restores event-time order in one aggregator partition.
//
// An aggregator partition receives entity events from every payments
// partition, through as many router tasks. Each source is in event-time
// order, but the sources interleave arbitrarily, so a device or an email
// domain shared by payments in two source partitions would see its events
// in arrival order, not event-time order. Velocity features computed in
// arrival order are not the features training saw. So the merger holds
// events back until every source has promised, with a watermark, that it
// will send nothing earlier, and then releases them in (DT, TransactionID,
// entity) order: per-channel watermarks with the minimum taken across
// channels, as in the Dataflow model and Flink.
//
// It also drops duplicates. A router that restarts re-sends everything after
// its last committed offset, and its records arrive at each aggregator
// partition in the order it sent them, so the highest (offset, entity) seen
// from each source is enough to recognize a resend, in constant memory.
type merger struct {
	wm   []Pos      // per source: nothing at or before this is still coming
	recv []seqMark  // per source: the highest (offset, entity) received
	buf  eventHeap  // received, not yet released
	last releaseKey // the last event released

	late, dups uint64
}

// seqMark is a position in one source's output: payments offset, then
// entity, the order the router sends a payment's entity events in.
type seqMark struct {
	Offset int64
	Entity uint8
}

func (a seqMark) after(b seqMark) bool {
	return a.Offset > b.Offset || (a.Offset == b.Offset && a.Entity > b.Entity)
}

// releaseKey is the order events leave the merger in: event time, then
// entity, then the payments record it came from. (DT, TransactionID) is
// unique for real payments, so the last two only make the order total, and
// so independent of heap shape, if the input ever breaks that.
type releaseKey struct {
	Pos    Pos
	Entity uint8
	Src    int32
	Offset int64
}

func (a releaseKey) less(b releaseKey) bool {
	if a.Pos != b.Pos {
		return a.Pos.Less(b.Pos)
	}
	if a.Entity != b.Entity {
		return a.Entity < b.Entity
	}
	if a.Src != b.Src {
		return a.Src < b.Src
	}
	return a.Offset < b.Offset
}

func keyOf(ev *EntityEvent) releaseKey {
	return releaseKey{PosOf(&ev.Txn), ev.Entity, ev.Src, ev.Offset}
}

func newMerger(sources int) *merger {
	m := &merger{wm: make([]Pos, sources), recv: make([]seqMark, sources), last: releaseKey{Pos: MinPos, Src: -1, Offset: -1}}
	for i := range m.wm {
		m.wm[i] = MinPos
		m.recv[i] = seqMark{Offset: -1}
	}
	return m
}

func (m *merger) checkSource(src int32) error {
	if src < 0 || int(src) >= len(m.wm) {
		return fmt.Errorf("stream: event from payments partition %d, but the merger tracks %d", src, len(m.wm))
	}
	return nil
}

// low is the minimum watermark: everything at or before it has arrived.
func (m *merger) low() Pos {
	low := MaxPos
	for _, w := range m.wm {
		if w.Less(low) {
			low = w
		}
	}
	return low
}

// add buffers an event, or drops it as a duplicate. An event at or before
// the minimum watermark that is not a duplicate is late, which a correct
// router never produces; it is counted and returned so the caller applies
// it at once (the state records a late event at its key's latest time).
func (m *merger) add(ev *EntityEvent) (late bool, err error) {
	if err := m.checkSource(ev.Src); err != nil {
		return false, err
	}
	mark := seqMark{ev.Offset, ev.Entity}
	if !mark.after(m.recv[ev.Src]) {
		m.dups++
		return false, nil
	}
	m.recv[ev.Src] = mark
	if k := keyOf(ev); !m.last.less(k) || !m.low().Less(k.Pos) {
		m.late++
		return true, nil
	}
	heap.Push(&m.buf, ev)
	return false, nil
}

// watermark records a source's progress. Watermarks only move forward: a
// restarted router may repeat an older one.
func (m *merger) watermark(w Watermark) error {
	if err := m.checkSource(w.Src); err != nil {
		return err
	}
	if m.wm[w.Src].Less(w.Pos) {
		m.wm[w.Src] = w.Pos
	}
	return nil
}

// release calls fn on every buffered event at or before the minimum
// watermark, in release order.
func (m *merger) release(fn func(*EntityEvent) error) error {
	low := m.low()
	for len(m.buf) > 0 {
		ev := m.buf[0]
		k := keyOf(ev)
		if low.Less(k.Pos) {
			return nil
		}
		heap.Pop(&m.buf)
		m.last = k
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

type eventHeap []*EntityEvent

func (h eventHeap) Len() int           { return len(h) }
func (h eventHeap) Less(i, j int) bool { return keyOf(h[i]).less(keyOf(h[j])) }
func (h eventHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)        { *h = append(*h, x.(*EntityEvent)) }
func (h *eventHeap) Pop() any {
	old := *h
	ev := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return ev
}

// encode writes the merger's state into a snapshot.
func (m *merger) encode(e *enc) {
	e.uvarint(uint64(len(m.wm)))
	for i := range m.wm {
		e.pos(m.wm[i])
		e.varint(m.recv[i].Offset)
		e.byte(m.recv[i].Entity)
	}
	e.pos(m.last.Pos)
	e.byte(m.last.Entity)
	e.varint(int64(m.last.Src))
	e.varint(m.last.Offset)
	e.uvarint(m.late)
	e.uvarint(m.dups)
	// Buffered events in release order, so equal mergers encode equally.
	evs := slices.Clone(m.buf)
	slices.SortFunc(evs, func(a, b *EntityEvent) int {
		if keyOf(a).less(keyOf(b)) {
			return -1
		}
		return 1 // release keys are unique
	})
	e.uvarint(uint64(len(evs)))
	for _, ev := range evs {
		e.bytes(appendEntityEvent(nil, ev))
	}
}

func decodeMerger(d *dec, sources int) (*merger, error) {
	n := int(d.uvarint())
	if d.err == nil && n != sources {
		return nil, fmt.Errorf("stream: snapshot tracks %d payments partitions, the topic has %d", n, sources)
	}
	m := newMerger(sources)
	for i := 0; i < n && d.err == nil; i++ {
		m.wm[i] = d.pos()
		m.recv[i] = seqMark{d.varint(), d.byte()}
	}
	m.last = releaseKey{Pos: d.pos(), Entity: d.byte()}
	m.last.Src, m.last.Offset = int32(d.varint()), d.varint()
	m.late = d.uvarint()
	m.dups = d.uvarint()
	k := d.uvarint()
	for i := uint64(0); i < k && d.err == nil; i++ {
		raw := d.raw()
		ev, _, err := decodeAggregatorInput(raw)
		if err != nil {
			return nil, err
		}
		if ev == nil {
			return nil, fmt.Errorf("stream: snapshot buffer holds a watermark")
		}
		m.buf = append(m.buf, ev)
	}
	heap.Init(&m.buf)
	return m, d.err
}
