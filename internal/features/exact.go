package features

import (
	"math"
	"unsafe"
)

// NewExact returns the reference state: every event of the last seven days,
// per key, in a deque. Its features are exact by definition, and every other
// implementation is measured against it. Its memory grows with traffic, so a
// card-testing burst on one device grows that device's deque without bound
// for a week. An idleTTL of 0 means DefaultIdleTTL.
func NewExact(idleTTL int64) State {
	ttl := checkTTL(idleTTL)
	return newKeyed(kindSpec{
		magic:    "riskgate/exact/v1",
		ttl:      ttl,
		newEntry: newExactEntry,
	})
}

type exactEvent struct {
	t     int64
	milli int64
	card  uint64
}

// exactEntry keeps a key's events with running totals per window.
//
// Every event gets a sequence number. For each window, start is the
// sequence number of the oldest event still inside it and count/milli total
// the events from start to the back. A read at a later time walks forward
// from start over the events that have since left the window and subtracts
// them from copies of the totals, so reads never write and cost only the
// number of events that expired since the last add. An add then commits the
// same walk. For the distinct-card window, cards maps each card to the
// sequence number of its latest event; an expiring event removes its card
// only if it is that card's latest one.
type exactEntry struct {
	ev       deque[exactEvent]
	base     uint64 // sequence number of the deque's front
	start    [NumWindows]uint64
	count    [NumWindows]int64
	milli    [NumWindows]int64
	cards    map[uint64]uint64 // nil unless the entity tracks distinct cards
	distinct int64
	first    int64
	last     int64
	started  bool
}

func newExactEntry(distinct bool) entry {
	e := &exactEntry{}
	if distinct {
		e.cards = make(map[uint64]uint64)
	}
	return e
}

func (e *exactEntry) end() uint64 { return e.base + uint64(e.ev.len()) }

func (e *exactEntry) read(now int64) Aggregates {
	a := Aggregates{Seen: true, First: e.first, Last: e.last, Distinct: math.NaN()}
	d := e.distinct
	end := e.end()
	for w := range NumWindows {
		c, m := e.count[w], e.milli[w]
		cutoff := now - windowSeconds[w]
		for s := e.start[w]; s < end; s++ {
			x := e.ev.at(int(s - e.base))
			if x.t > cutoff {
				break
			}
			c--
			m -= x.milli
			if w == distinctWindow && e.cards != nil && x.card != NoCard && e.cards[x.card] == s {
				d--
			}
		}
		a.Count[w] = float64(c)
		a.Sum[w] = float64(m) / 1000
	}
	if e.cards != nil {
		a.Distinct = float64(d)
	}
	return a
}

// expire commits the walk read does: events at or before now - W leave
// window W, and events outside the longest window leave the deque.
func (e *exactEntry) expire(now int64) {
	end := e.end()
	for w := range NumWindows {
		cutoff := now - windowSeconds[w]
		for e.start[w] < end && e.ev.at(int(e.start[w]-e.base)).t <= cutoff {
			e.dropOldest(w)
		}
	}
	for e.base < e.start[NumWindows-1] {
		e.ev.popFront()
		e.base++
	}
}

// dropOldest removes window w's oldest event from the window's totals.
func (e *exactEntry) dropOldest(w int) {
	s := e.start[w]
	x := e.ev.at(int(s - e.base))
	e.count[w]--
	e.milli[w] -= x.milli
	if w == distinctWindow && e.cards != nil && x.card != NoCard && e.cards[x.card] == s {
		delete(e.cards, x.card)
		e.distinct--
	}
	e.start[w]++
}

func (e *exactEntry) add(ev Event) {
	if e.started && ev.Time < e.last {
		ev.Time = e.last // late event: record it at the key's latest time
	}
	e.expire(ev.Time)
	e.push(exactEvent{t: ev.Time, milli: ev.Milli, card: ev.Card})
	if !e.started {
		e.first, e.started = ev.Time, true
	}
	e.last = ev.Time
}

// push appends an event already known to be inside every window.
func (e *exactEntry) push(x exactEvent) {
	seq := e.end()
	e.ev.pushBack(x)
	for w := range NumWindows {
		e.count[w]++
		e.milli[w] += x.milli
	}
	if e.cards != nil && x.card != NoCard {
		if _, ok := e.cards[x.card]; !ok {
			e.distinct++
		}
		e.cards[x.card] = seq
	}
}

func (e *exactEntry) lastSeen() int64 { return e.last }

func (e *exactEntry) bytes() int64 {
	b := int64(unsafe.Sizeof(*e)) + int64(e.ev.cap())*int64(unsafe.Sizeof(exactEvent{}))
	if e.cards != nil {
		b += 48 + int64(len(e.cards))*(8+8+1)*8/7
	}
	return b
}

// encode writes the events and window starts. Totals and the card map are
// derived data and are rebuilt on decode, which also checks them.
func (e *exactEntry) encode(enc *encoder) {
	enc.varint(e.first)
	enc.varint(e.last)
	enc.uvarint(uint64(e.ev.len()))
	prev := e.first
	for i := range e.ev.len() {
		x := e.ev.at(i)
		enc.varint(x.t - prev)
		prev = x.t
		enc.varint(x.milli)
		enc.u64(x.card)
	}
	for w := range NumWindows {
		enc.uvarint(e.start[w] - e.base)
	}
}

func (e *exactEntry) decode(d *decoder) {
	e.first = d.varint()
	e.last = d.varint()
	e.started = true
	n := d.count(1 << 32)
	var starts [NumWindows]uint64
	evs := make([]exactEvent, 0, min(n, 1<<16))
	prev := e.first
	for range n {
		if d.err != nil {
			return
		}
		x := exactEvent{t: prev + d.varint(), milli: d.varint(), card: d.u64()}
		prev = x.t
		evs = append(evs, x)
	}
	for w := range NumWindows {
		starts[w] = d.uvarint()
		if starts[w] > uint64(n) || (w > 0 && starts[w] > starts[w-1]) {
			d.fail(errSnapshot)
		}
	}
	if d.err != nil {
		return
	}
	// Rebuild: every window starts at the front with all events, then the
	// events before each window's start are expired as expire would.
	for _, x := range evs {
		e.push(x)
	}
	for w := range NumWindows {
		for e.start[w] < starts[w] {
			e.dropOldest(w)
		}
	}
	if starts[NumWindows-1] != 0 {
		d.fail(errSnapshot) // the longest window always starts at the front
	}
}
