package features

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sort"
	"unsafe"
)

// BucketPlan is the bucket width, in seconds, for each window. Each width
// must divide its window.
type BucketPlan [NumWindows]int64

// DefaultBucketPlan uses one-minute buckets for the hour, fifteen-minute
// buckets for the day, and hourly buckets for the week.
var DefaultBucketPlan = BucketPlan{60, 900, 3600}

// DefaultDistinctCap is where Bucketed stops tracking new distinct cards.
const DefaultDistinctCap = 1024

// BucketedConfig configures NewBucketed. Zero fields take defaults.
type BucketedConfig struct {
	Plan        BucketPlan
	DistinctCap int
	IdleTTL     int64
}

// NewBucketed returns a state that keeps, per key and per window, a ring of
// fixed-width time buckets holding a count and a sum. Memory per key is
// bounded by the number of buckets (at most 61 + 97 + 169 under the default
// plan) however many payments the key makes, and only non-empty buckets are
// stored, so a key with three payments costs three buckets per window.
//
// The approximation is at the trailing edge. At time now, window W covers
// every bucket from floor((now - W) / width) to the current one, so it
// includes every event the exact window includes plus events in the oldest
// bucket that are up to one bucket width too old. Counts and sums therefore
// never undercount and overcount by at most one bucket's contents. Distinct
// cards use the same buckets, and stop growing at DistinctCap cards (a
// saturated key reports the cap) so a card-testing attack cannot grow a
// device's card set without bound.
func NewBucketed(cfg BucketedConfig) State {
	if cfg.Plan == (BucketPlan{}) {
		cfg.Plan = DefaultBucketPlan
	}
	if cfg.DistinctCap == 0 {
		cfg.DistinctCap = DefaultDistinctCap
	}
	bc := &bucketCfg{cap: int64(cfg.DistinctCap)}
	for w, width := range cfg.Plan {
		if width <= 0 || windowSeconds[w]%width != 0 {
			panic(fmt.Sprintf("features: bucket width %d does not divide window %ds", width, windowSeconds[w]))
		}
		bc.width[w] = width
		bc.span[w] = windowSeconds[w] / width
	}
	if cfg.DistinctCap < 1 {
		panic("features: distinct cap must be positive")
	}
	return newKeyed(kindSpec{
		magic:  "riskgate/bucketed/v1",
		ttl:    checkTTL(cfg.IdleTTL),
		config: append(slices.Clone(cfg.Plan[:]), int64(cfg.DistinctCap)),
		newEntry: func(distinct bool) entry {
			e := &bucketEntry{cfg: bc}
			if distinct {
				e.cards = make(map[uint64]int64)
			}
			return e
		},
	})
}

type bucketCfg struct {
	width [NumWindows]int64
	span  [NumWindows]int64 // window / width
	cap   int64
}

// oldest is the index of the oldest bucket inside window w at time now.
func (c *bucketCfg) oldest(w int, now int64) int64 { return floorDiv(now, c.width[w]) - c.span[w] }

type bucket struct {
	idx   int64 // floor(time / width)
	milli int64
	count uint32
	// lastCards counts the cards whose latest event in the distinct window
	// falls in this bucket, so a bucket leaving the window takes exactly
	// those cards out of the distinct count.
	lastCards uint32
}

type bucketEntry struct {
	cfg      *bucketCfg
	win      [NumWindows]deque[bucket]
	count    [NumWindows]int64
	milli    [NumWindows]int64
	cards    map[uint64]int64 // card -> bucket of its latest event; may hold expired entries
	distinct int64
	first    int64
	last     int64
	started  bool
}

func (e *bucketEntry) read(now int64) Aggregates {
	a := Aggregates{Seen: true, First: e.first, Last: e.last, Distinct: math.NaN()}
	d := e.distinct
	for w := range NumWindows {
		c, m := e.count[w], e.milli[w]
		lo := e.cfg.oldest(w, now)
		q := &e.win[w]
		for i := 0; i < q.len() && q.at(i).idx < lo; i++ {
			b := q.at(i)
			c -= int64(b.count)
			m -= b.milli
			if w == distinctWindow {
				d -= int64(b.lastCards)
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

func (e *bucketEntry) expire(now int64) {
	for w := range NumWindows {
		lo := e.cfg.oldest(w, now)
		q := &e.win[w]
		for q.len() > 0 && q.front().idx < lo {
			b := q.front()
			e.count[w] -= int64(b.count)
			e.milli[w] -= b.milli
			if w == distinctWindow {
				e.distinct -= int64(b.lastCards)
			}
			q.popFront()
		}
	}
}

func (e *bucketEntry) add(ev Event) {
	if e.started && ev.Time < e.last {
		ev.Time = e.last
	}
	e.expire(ev.Time)
	for w := range NumWindows {
		e.addToBucket(w, floorDiv(ev.Time, e.cfg.width[w]), 1, ev.Milli)
	}
	if e.cards != nil && ev.Card != NoCard {
		e.addCard(ev.Card)
	}
	if !e.started {
		e.first, e.started = ev.Time, true
	}
	e.last = ev.Time
}

func (e *bucketEntry) addToBucket(w int, idx int64, count uint32, milli int64) {
	q := &e.win[w]
	if q.len() == 0 || q.back().idx != idx {
		q.pushBack(bucket{idx: idx})
	}
	b := q.back()
	b.count += count
	b.milli += milli
	e.count[w] += int64(count)
	e.milli[w] += milli
}

// addCard records card in the newest distinct-window bucket.
func (e *bucketEntry) addCard(card uint64) {
	q := &e.win[distinctWindow]
	cur := q.back()
	old, ok := e.cards[card]
	switch {
	case ok && old >= q.front().idx: // already live: move it to the newest bucket
		if old != cur.idx {
			e.find(old).lastCards--
			cur.lastCards++
			e.cards[card] = cur.idx
		}
	case e.distinct < e.cfg.cap:
		e.cards[card] = cur.idx
		cur.lastCards++
		e.distinct++
	default:
		// Saturated: the card goes uncounted, and the key reports the cap.
	}
	// Entries for expired buckets linger until the map is mostly stale. The
	// sweep then costs O(map) after at least that many new cards, so it is
	// amortized O(1), and the map never exceeds about twice the cap.
	if len(e.cards) > 2*int(e.distinct)+64 {
		lo := q.front().idx
		for c, idx := range e.cards {
			if idx < lo {
				delete(e.cards, c)
			}
		}
	}
}

// find returns the distinct-window bucket with index idx, which must exist.
func (e *bucketEntry) find(idx int64) *bucket {
	q := &e.win[distinctWindow]
	i := sort.Search(q.len(), func(i int) bool { return q.at(i).idx >= idx })
	return q.at(i)
}

func (e *bucketEntry) lastSeen() int64 { return e.last }

func (e *bucketEntry) bytes() int64 {
	b := int64(unsafe.Sizeof(*e))
	for w := range NumWindows {
		b += int64(e.win[w].cap()) * int64(unsafe.Sizeof(bucket{}))
	}
	if e.cards != nil {
		b += 48 + int64(len(e.cards))*(8+8+1)*8/7
	}
	return b
}

// encode writes the buckets and the live card map. Totals and lastCards are
// rebuilt on decode; expired card entries are dropped, as they never affect
// a result.
func (e *bucketEntry) encode(enc *encoder) {
	enc.varint(e.first)
	enc.varint(e.last)
	for w := range NumWindows {
		q := &e.win[w]
		enc.uvarint(uint64(q.len()))
		for i := range q.len() {
			b := q.at(i)
			enc.varint(b.idx)
			enc.uvarint(uint64(b.count))
			enc.varint(b.milli)
		}
	}
	if e.cards == nil {
		return
	}
	type cardAt struct {
		card uint64
		idx  int64
	}
	var live []cardAt
	if q := &e.win[distinctWindow]; q.len() > 0 {
		for c, idx := range e.cards {
			if idx >= q.front().idx {
				live = append(live, cardAt{c, idx})
			}
		}
	}
	slices.SortFunc(live, func(a, b cardAt) int { return cmp.Compare(a.card, b.card) })
	enc.uvarint(uint64(len(live)))
	for _, c := range live {
		enc.u64(c.card)
		enc.varint(c.idx)
	}
}

func (e *bucketEntry) decode(d *decoder) {
	e.first = d.varint()
	e.last = d.varint()
	e.started = true
	for w := range NumWindows {
		n := d.count(uint64(e.cfg.span[w]) + 1)
		prev := int64(math.MinInt64)
		for range n {
			idx, count, milli := d.varint(), d.uvarint(), d.varint()
			if d.err != nil {
				return
			}
			if idx <= prev || count == 0 || count > math.MaxUint32 {
				d.fail(errSnapshot)
				return
			}
			prev = idx
			e.addToBucket(w, idx, uint32(count), milli)
		}
	}
	if e.cards == nil {
		return
	}
	n := d.count(uint64(e.cfg.cap))
	for range n {
		card, idx := d.u64(), d.varint()
		if d.err != nil {
			return
		}
		q := &e.win[distinctWindow]
		if q.len() == 0 || idx < q.front().idx || idx > q.back().idx {
			d.fail(errSnapshot)
			return
		}
		b := e.find(idx)
		if b.idx != idx {
			d.fail(errSnapshot)
			return
		}
		b.lastCards++
		e.cards[card] = idx
		e.distinct++
	}
}
