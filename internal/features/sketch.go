package features

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
	"slices"
	"unsafe"
)

// DefaultSketchPlan is coarser than DefaultBucketPlan because every bucket
// of a sketch costs a full count-min table: 5-minute buckets for the hour,
// hourly for the day, 6-hourly for the week.
var DefaultSketchPlan = BucketPlan{300, 3600, 21600}

// SketchConfig configures NewSketch. Zero fields take defaults.
type SketchConfig struct {
	Depth        int        // count-min rows (default 4, at most 8)
	Width        int        // count-min columns, a power of two (default 1<<14)
	Plan         BucketPlan // bucket widths (default DefaultSketchPlan)
	HLLPrecision int        // log2 of HyperLogLog registers (default 6, range 4-8)
	HLLDepth     int        // HyperLogLog rows (default 2, at most 8)
	HLLWidth     int        // HyperLogLog columns per row, a power of two (default 1024)
	// RecencySlots is the size of the first/last-seen table, a power of
	// two and at least recencyWays (default 1<<18, about 6 MB). Size it
	// above the number of keys active within IdleTTL; keys beyond that are
	// forgotten early (see Sketch).
	RecencySlots int
	IdleTTL      int64
}

const maxSketchDepth = 8

func (c *SketchConfig) setDefaults() {
	if c.Depth == 0 {
		c.Depth = 4
	}
	if c.Width == 0 {
		c.Width = 1 << 14
	}
	if c.Plan == (BucketPlan{}) {
		c.Plan = DefaultSketchPlan
	}
	if c.HLLPrecision == 0 {
		c.HLLPrecision = 6
	}
	if c.HLLDepth == 0 {
		c.HLLDepth = 2
	}
	if c.HLLWidth == 0 {
		c.HLLWidth = 1024
	}
	if c.RecencySlots == 0 {
		c.RecencySlots = 1 << 18
	}
	c.IdleTTL = checkTTL(c.IdleTTL)
	pow2 := func(n int) bool { return n > 0 && n&(n-1) == 0 }
	switch {
	case c.Depth < 1 || c.Depth > maxSketchDepth || c.HLLDepth < 1 || c.HLLDepth > maxSketchDepth:
		panic("features: sketch depth out of range")
	case !pow2(c.Width) || !pow2(c.HLLWidth) || c.Width > 1<<24 || c.HLLWidth > 1<<20:
		panic("features: sketch widths must be powers of two")
	case c.HLLPrecision < 4 || c.HLLPrecision > 8:
		panic("features: HLL precision out of range")
	case !pow2(c.RecencySlots) || c.RecencySlots < recencyWays || c.RecencySlots > 1<<26:
		panic("features: recency slots must be a power of two, at least 8")
	}
	for w, width := range c.Plan {
		if width <= 0 || windowSeconds[w]%width != 0 {
			panic(fmt.Sprintf("features: bucket width %d does not divide window %ds", width, windowSeconds[w]))
		}
	}
}

func (c *SketchConfig) values() []int64 {
	v := []int64{int64(c.Depth), int64(c.Width), int64(c.HLLPrecision), int64(c.HLLDepth), int64(c.HLLWidth), int64(c.RecencySlots)}
	return append(v, c.Plan[:]...)
}

// Sketch keeps constant-size summaries shared by all keys:
//
//   - Counts and sums: for each window, a ring of count-min sketches, one per
//     time bucket, plus their running sum. Estimates take the minimum over
//     rows, so they never undercount; colliding keys push them up by an
//     amount that grows with the total traffic in the window, not with the
//     key's own. The window edge behaves exactly as in Bucketed with the
//     same plan.
//   - Distinct cards per device and per email domain: HyperLogLog
//     registers, per hourly bucket and hashed column, merged over the day at
//     read time. With the default 64 registers the standard error is about
//     13%, but small counts use linear counting and are close to exact; a
//     column shared with another key adds that key's cards, and the minimum
//     over HLLDepth independent rows limits the damage.
//   - First and last seen: not a sketch. Whether a key has been seen at all
//     is membership, and a shared min/max sketch answers membership wrongly
//     in the harmful direction: once every cell has been touched by some
//     key, every never-seen key reads as seen, with another key's times.
//     (That was a bug; see BUGLOG.md.) Instead a fixed-size, 8-way
//     set-associative table stores each key's full 64-bit hash with its
//     first and last times. A key that has been idle for IdleTTL reads as
//     unseen and starts over on its next event, exactly as in Exact. When
//     a set is full, the entry seen least recently is dropped. The error is
//     one-sided: a key can be forgotten early (it reads as unseen, or with
//     a first-seen time that is too recent), but a key is never reported
//     as seen when it was not, except for a full 64-bit hash collision
//     (about n^2/2^64 over n keys). When Seen is reported, Last is exact
//     and First is never earlier than the truth, so seconds_since_first
//     never overestimates. With fewer live keys than about half the
//     slots, drops are rare and the table matches Exact.
//
// Memory is fixed at construction (about 70 MB with defaults) and does not
// depend on the number of keys; Stats reports Keys as -1.
type Sketch struct {
	cfg     SketchConfig
	rings   [NumWindows]cmsRing
	seen    []recencySlot // RecencySlots entries in sets of recencyWays
	hll     [2]hllRing
	latest  int64 // newest event time; later events never go backward
	started bool
}

// NewSketch builds a sketch state.
func NewSketch(cfg SketchConfig) *Sketch {
	cfg.setDefaults()
	s := &Sketch{cfg: cfg}
	cells := cfg.Depth * cfg.Width
	for w := range NumWindows {
		s.rings[w] = newCMSRing(cfg.Plan[w], windowSeconds[w]/cfg.Plan[w], cells)
	}
	s.seen = make([]recencySlot, cfg.RecencySlots)
	for i := range s.hll {
		width := cfg.Plan[distinctWindow]
		s.hll[i] = newHLLRing(width, windowSeconds[distinctWindow]/width, cfg.HLLDepth*cfg.HLLWidth<<cfg.HLLPrecision)
	}
	return s
}

// recencyWays is the associativity of the first/last-seen table: a set
// of 8 slots is 192 bytes, three cache lines.
const recencyWays = 8

// recencySlot is one key's first and last event times. tag is the key's
// hash with the low bit set, so 0 marks an empty slot.
type recencySlot struct {
	tag         uint64
	first, last int64
}

const recencySeed = 0x6a09e667f3bcc909 // digits of sqrt(2)

func recencyTag(h uint64) uint64 { return h | 1 }

// recencySet returns the slots of the set key hash h maps to.
func (s *Sketch) recencySet(h uint64) []recencySlot {
	sets := uint64(len(s.seen) / recencyWays)
	i := int(mix64(h^recencySeed)&(sets-1)) * recencyWays
	return s.seen[i : i+recencyWays]
}

// lookup returns the live slot for h at now, or nil.
func (s *Sketch) lookup(h uint64, now int64) *recencySlot {
	tag := recencyTag(h)
	set := s.recencySet(h)
	for i := range set {
		if set[i].tag == tag {
			if now-set[i].last < s.cfg.IdleTTL {
				return &set[i]
			}
			return nil
		}
	}
	return nil
}

// touch records an event at t for h. A key that is new, or idle for the
// TTL, starts over at t. A new key takes an empty slot, or else the slot
// seen least recently, which is idle before any other in the set.
func (s *Sketch) touch(h uint64, t int64) {
	tag := recencyTag(h)
	set := s.recencySet(h)
	victim := -1
	for i := range set {
		switch {
		case set[i].tag == tag:
			if t-set[i].last >= s.cfg.IdleTTL {
				set[i].first = t
			}
			set[i].last = t
			return
		case set[i].tag == 0:
			if victim < 0 || set[victim].tag != 0 {
				victim = i
			}
		case victim < 0 || set[victim].tag != 0 && set[i].last < set[victim].last:
			victim = i
		}
	}
	set[victim] = recencySlot{tag: tag, first: t, last: t}
}

// hllIndex maps an entity to its HyperLogLog ring.
func hllIndex(e Entity) int {
	if e == Device {
		return 0
	}
	return 1
}

// Seeds decorrelate the rows. Any fixed odd constants will do; these are
// digits of pi.
var rowSeeds = [maxSketchDepth]uint64{
	0x243f6a8885a308d3, 0x13198a2e03707344, 0xa4093822299f31d0, 0x082efa98ec4e6c89,
	0x452821e638d01377, 0xbe5466cf34e90c6c, 0xc0ac29b7c97c50dd, 0x3f84d5b5b5470917,
}

const hllSeed = 0x9216d5d98979fb1b

// cells returns the count-min cell of each row for key hash h.
func (s *Sketch) cells(h uint64) (c [maxSketchDepth]int) {
	for r := range s.cfg.Depth {
		c[r] = r*s.cfg.Width + int(mix64(h^rowSeeds[r])&uint64(s.cfg.Width-1))
	}
	return c
}

func (s *Sketch) Read(k Key, now int64) Aggregates {
	h := k.Hash()
	cells := s.cells(h)
	a := Aggregates{Distinct: math.NaN()}
	for w := range NumWindows {
		a.Count[w], a.Sum[w] = s.rings[w].estimate(cells[:s.cfg.Depth], now)
	}
	if e := s.lookup(h, now); e != nil {
		a.Seen, a.First, a.Last = true, e.first, e.last
	}
	if k.Entity.tracksDistinct() {
		cols := s.hllCols(h)
		a.Distinct = s.hll[hllIndex(k.Entity)].estimate(cols[:s.cfg.HLLDepth], now, s.cfg.HLLPrecision)
	}
	return a
}

// hllCols returns the register offset of each HyperLogLog row for key hash
// h. It returns an array, not a slice, so that reads do not allocate.
func (s *Sketch) hllCols(h uint64) (cols [maxSketchDepth]int) {
	regs := 1 << s.cfg.HLLPrecision
	for r := range s.cfg.HLLDepth {
		cols[r] = (r*s.cfg.HLLWidth + int(mix64(h^rowSeeds[r]^hllSeed)&uint64(s.cfg.HLLWidth-1))) * regs
	}
	return cols
}

func (s *Sketch) Add(k Key, ev Event) {
	if s.started && ev.Time < s.latest {
		ev.Time = s.latest
	}
	s.latest, s.started = ev.Time, true
	h := k.Hash()
	cells := s.cells(h)
	for w := range NumWindows {
		r := &s.rings[w]
		r.advance(floorDiv(ev.Time, r.width))
		r.add(cells[:s.cfg.Depth], ev.Milli)
	}
	s.touch(h, ev.Time)
	if k.Entity.tracksDistinct() && ev.Card != NoCard {
		r := &s.hll[hllIndex(k.Entity)]
		r.advance(floorDiv(ev.Time, r.width))
		cols := s.hllCols(h)
		r.add(cols[:s.cfg.HLLDepth], ev.Card, s.cfg.HLLPrecision)
	}
}

func (s *Sketch) ReadAdd(k Key, ev Event) Aggregates {
	a := s.Read(k, ev.Time)
	s.Add(k, ev)
	return a
}

// EvictIdle does nothing: a sketch forgets by rotating buckets.
func (s *Sketch) EvictIdle(int64) int { return 0 }

func (s *Sketch) Stats() Stats {
	b := int64(len(s.seen)) * int64(unsafe.Sizeof(recencySlot{}))
	for w := range s.rings {
		b += s.rings[w].bytes()
	}
	for i := range s.hll {
		b += int64(len(s.hll[i].regs)) + int64(len(s.hll[i].idx))*8
	}
	return Stats{Keys: -1, Bytes: b}
}

// noBucket marks an empty ring slot.
const noBucket = math.MinInt64

// cms is a count-min table of (count, thousandths) pairs.
type cms struct {
	count []uint32
	milli []int64
}

func newCMS(cells int) cms { return cms{count: make([]uint32, cells), milli: make([]int64, cells)} }

// cmsRing holds one window: span+1 bucket tables (buckets cur-span..cur,
// which is exactly the window's reach at any time in bucket cur) and their
// cell-wise sum.
//
// Rotating a bucket out must subtract its table from the sum. Doing that
// densely would touch every cell of a 64K-cell table every five minutes, so
// each slot keeps a list of the cells it has touched and rotation costs only
// those.
type cmsRing struct {
	width int64
	span  int64
	slots []cms
	idx   []int64
	dirty [][]int32
	sum   cms
	cur   int64
}

func newCMSRing(width, span int64, cells int) cmsRing {
	r := cmsRing{width: width, span: span, sum: newCMS(cells), cur: noBucket}
	r.slots = make([]cms, span+1)
	r.idx = make([]int64, span+1)
	r.dirty = make([][]int32, span+1)
	for i := range r.slots {
		r.slots[i] = newCMS(cells)
		r.idx[i] = noBucket
	}
	return r
}

func (r *cmsRing) slot(b int64) int {
	n := r.span + 1
	return int(((b % n) + n) % n)
}

// clear subtracts slot i from the sum and empties it.
func (r *cmsRing) clear(i int) {
	sl := &r.slots[i]
	for _, c := range r.dirty[i] {
		r.sum.count[c] -= sl.count[c]
		r.sum.milli[c] -= sl.milli[c]
		sl.count[c], sl.milli[c] = 0, 0
	}
	r.dirty[i] = r.dirty[i][:0]
	r.idx[i] = noBucket
}

// advance makes bn the newest bucket, rotating out everything older than
// bn - span.
func (r *cmsRing) advance(bn int64) {
	switch {
	case r.cur != noBucket && bn <= r.cur:
		return
	case r.cur == noBucket || bn-r.cur > r.span:
		for i := range r.slots {
			r.clear(i)
		}
	default:
		for b := r.cur + 1; b <= bn; b++ {
			r.clear(r.slot(b))
		}
	}
	r.idx[r.slot(bn)] = bn
	r.cur = bn
}

func (r *cmsRing) add(cells []int, milli int64) {
	i := r.slot(r.cur)
	sl := &r.slots[i]
	for _, c := range cells {
		if sl.count[c] == 0 {
			r.dirty[i] = append(r.dirty[i], int32(c))
		}
		sl.count[c]++
		sl.milli[c] += milli
		r.sum.count[c]++
		r.sum.milli[c] += milli
	}
}

// estimate reads the window at now without committing any rotation: slots
// that have left the window since the last add are subtracted on the fly.
func (r *cmsRing) estimate(cells []int, now int64) (count, sum float64) {
	if r.cur == noBucket {
		return 0, 0
	}
	bn := floorDiv(now, r.width)
	lo, hi := r.cur-r.span, min(r.cur, bn-r.span-1) // expired but uncommitted buckets
	minCount, minMilli := int64(math.MaxInt64), int64(math.MaxInt64)
	for _, c := range cells {
		n, m := int64(r.sum.count[c]), r.sum.milli[c]
		for b := lo; b <= hi; b++ {
			if i := r.slot(b); r.idx[i] == b {
				n -= int64(r.slots[i].count[c])
				m -= r.slots[i].milli[c]
			}
		}
		minCount, minMilli = min(minCount, n), min(minMilli, m)
	}
	return float64(minCount), float64(minMilli) / 1000
}

func (r *cmsRing) bytes() int64 {
	cells := int64(len(r.sum.count))
	b := (cells * 12) * int64(len(r.slots)+1)
	for _, d := range r.dirty {
		b += int64(cap(d)) * 4
	}
	return b + int64(len(r.idx))*8
}

// hllRing is a ring of HyperLogLog tables for the distinct-card window. Each
// slot holds rows x columns x registers bytes.
type hllRing struct {
	width int64
	span  int64
	regs  []uint8
	idx   []int64
	cur   int64
	size  int // bytes per slot
}

func newHLLRing(width, span int64, size int) hllRing {
	r := hllRing{width: width, span: span, cur: noBucket, size: size}
	r.regs = make([]uint8, int(span+1)*size)
	r.idx = make([]int64, span+1)
	for i := range r.idx {
		r.idx[i] = noBucket
	}
	return r
}

func (r *hllRing) slot(b int64) int {
	n := r.span + 1
	return int(((b % n) + n) % n)
}

func (r *hllRing) advance(bn int64) {
	switch {
	case r.cur != noBucket && bn <= r.cur:
		return
	case r.cur == noBucket || bn-r.cur > r.span:
		clear(r.regs)
		for i := range r.idx {
			r.idx[i] = noBucket
		}
	default:
		for b := r.cur + 1; b <= bn; b++ {
			i := r.slot(b)
			clear(r.regs[i*r.size : (i+1)*r.size])
			r.idx[i] = noBucket
		}
	}
	r.idx[r.slot(bn)] = bn
	r.cur = bn
}

// add records card in each row's column of the newest slot.
func (r *hllRing) add(cols []int, card uint64, p int) {
	h := mix64(card ^ hllSeed)
	j := int(h >> (64 - p))
	rank := uint8(bits.LeadingZeros64(h<<p|1<<(p-1)) + 1)
	base := r.slot(r.cur) * r.size
	for _, c := range cols {
		if reg := &r.regs[base+c+j]; *reg < rank {
			*reg = rank
		}
	}
}

// estimate merges the slots inside the window at now and returns the
// minimum estimate over rows.
func (r *hllRing) estimate(cols []int, now int64, p int) float64 {
	if r.cur == noBucket {
		return 0
	}
	bn := floorDiv(now, r.width)
	m := 1 << p
	best := math.Inf(1)
	var merged [256]uint8
	for _, c := range cols {
		clear(merged[:m])
		for b := max(r.cur-r.span, bn-r.span); b <= r.cur; b++ {
			i := r.slot(b)
			if r.idx[i] != b {
				continue
			}
			maxBytes(merged[:m], r.regs[i*r.size+c:i*r.size+c+m])
		}
		best = min(best, hllEstimate(merged[:m]))
	}
	return best
}

// maxBytes sets dst[j] = max(dst[j], src[j]) eight bytes at a time. It
// relies on every register being below 128, which holds because a rank is
// at most 65 - precision: with the top bit of each byte forced on in a and
// clear in b, the top bit of (a|H) - b is set exactly where a >= b, and no
// borrow crosses a byte. len(dst) is a multiple of 8 (at least 16).
func maxBytes(dst, src []uint8) {
	const h = 0x8080808080808080
	for j := 0; j+8 <= len(dst); j += 8 {
		a := binary.LittleEndian.Uint64(dst[j:])
		b := binary.LittleEndian.Uint64(src[j:])
		ge := (((a | h) - b) & h) >> 7 * 0xff // 0xff in each byte where a >= b
		binary.LittleEndian.PutUint64(dst[j:], a&ge|b&^ge)
	}
}

// inversePow2[v] is 2^-v for every possible register value.
var inversePow2 = func() (t [65]float64) {
	for v := range t {
		t[v] = math.Ldexp(1, -v)
	}
	return t
}()

// hllEstimate is the HyperLogLog estimate (Flajolet et al., 2007) with
// linear counting for small cardinalities. With 64-bit hashes the
// large-range correction is unnecessary.
func hllEstimate(regs []uint8) float64 {
	m := float64(len(regs))
	var sum float64
	zeros := 0
	for _, v := range regs {
		sum += inversePow2[v]
		if v == 0 {
			zeros++
		}
	}
	var alpha float64
	switch len(regs) {
	case 16:
		alpha = 0.673
	case 32:
		alpha = 0.697
	case 64:
		alpha = 0.709
	default:
		alpha = 0.7213 / (1 + 1.079/m)
	}
	e := alpha * m * m / sum
	if e <= 2.5*m && zeros > 0 {
		return m * math.Log(m/float64(zeros))
	}
	return e
}

// v2 replaced v1's min/max first/last-seen sketches with the recency table.
const sketchMagic = "riskgate/sketch/v2"

// Snapshot writes the non-empty cells of every table, in index order. The
// running sums are rebuilt on restore.
func (s *Sketch) Snapshot(w io.Writer) error {
	enc := newEncoder(w)
	enc.str(sketchMagic)
	enc.varint(s.cfg.IdleTTL)
	for _, v := range s.cfg.values() {
		enc.varint(v)
	}
	enc.varint(s.latest)
	enc.uvarint(boolInt(s.started))
	for w := range s.rings {
		r := &s.rings[w]
		enc.varint(r.cur)
		for i := range r.slots {
			enc.varint(r.idx[i])
			cells := slices.Clone(r.dirty[i])
			slices.Sort(cells)
			enc.uvarint(uint64(len(cells)))
			for _, c := range cells {
				enc.uvarint(uint64(c))
				enc.uvarint(uint64(r.slots[i].count[c]))
				enc.varint(r.slots[i].milli[c])
			}
		}
	}
	n := 0
	for i := range s.seen {
		if s.seen[i].tag != 0 {
			n++
		}
	}
	enc.uvarint(uint64(n))
	for i, e := range s.seen {
		if e.tag != 0 {
			enc.uvarint(uint64(i))
			enc.uvarint(e.tag)
			enc.varint(e.first)
			enc.varint(e.last)
		}
	}
	for i := range s.hll {
		r := &s.hll[i]
		enc.varint(r.cur)
		for _, b := range r.idx {
			enc.varint(b)
		}
		nz := 0
		for _, v := range r.regs {
			if v != 0 {
				nz++
			}
		}
		enc.uvarint(uint64(nz))
		for j, v := range r.regs {
			if v != 0 {
				enc.uvarint(uint64(j))
				enc.uvarint(uint64(v))
			}
		}
	}
	return enc.flush()
}

func boolInt(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// Restore replaces the sketch's contents with a snapshot taken from a
// sketch with the same configuration.
func (s *Sketch) Restore(r io.Reader) error {
	fresh := NewSketch(s.cfg)
	if err := fresh.decode(newDecoder(r)); err != nil {
		return err
	}
	*s = *fresh
	return nil
}

func (s *Sketch) decode(d *decoder) error {
	if magic := d.str(); d.err == nil && magic != sketchMagic {
		return fmt.Errorf("features: snapshot is of a %q state, this is a %q state", magic, sketchMagic)
	}
	d.expectInt("idle TTL", s.cfg.IdleTTL)
	for i, v := range s.cfg.values() {
		d.expectInt(fmt.Sprintf("sketch config[%d]", i), v)
	}
	s.latest = d.varint()
	s.started = d.uvarint() == 1
	cells := uint64(s.cfg.Depth * s.cfg.Width)
	for w := range s.rings {
		r := &s.rings[w]
		r.cur = d.varint()
		for i := range r.slots {
			r.idx[i] = d.varint()
			n := d.count(cells)
			for range n {
				c, count, milli := d.count(cells-1), d.uvarint(), d.varint()
				if d.err != nil {
					return d.err
				}
				if count == 0 || count > math.MaxUint32 || r.slots[i].count[c] != 0 {
					return errSnapshot
				}
				r.dirty[i] = append(r.dirty[i], int32(c))
				r.slots[i].count[c], r.slots[i].milli[c] = uint32(count), milli
				r.sum.count[c] += uint32(count)
				r.sum.milli[c] += milli
			}
		}
	}
	slots := uint64(len(s.seen))
	n := d.count(slots)
	for range n {
		i, tag, first, last := d.count(slots-1), d.uvarint(), d.varint(), d.varint()
		if d.err != nil {
			return d.err
		}
		if tag&1 == 0 || s.seen[i].tag != 0 || first > last {
			return errSnapshot
		}
		s.seen[i] = recencySlot{tag: tag, first: first, last: last}
	}
	for i := range s.hll {
		r := &s.hll[i]
		r.cur = d.varint()
		for j := range r.idx {
			r.idx[j] = d.varint()
		}
		n := d.count(uint64(len(r.regs)))
		for range n {
			j, v := d.count(uint64(len(r.regs)-1)), d.count(64)
			if d.err == nil {
				r.regs[j] = uint8(v)
			}
		}
	}
	return d.end()
}
