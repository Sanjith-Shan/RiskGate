package webhook

import (
	"container/list"
	"slices"
	"sync"
	"time"
)

// Defaults for NewDeduper.
const (
	// DefaultDedupeTTL covers Clearinghouse's retry schedule (exponential
	// backoff for about 3 simulated days) plus a day of margin for clock
	// skew and a late manual replay.
	DefaultDedupeTTL = 96 * time.Hour
	// DefaultDedupeMaxEntries caps memory at roughly 100-150 MB of ids
	// (an event id is ~30 bytes; map and list overhead dominate).
	DefaultDedupeMaxEntries = 1 << 20
)

// DedupeStatus is the outcome of Deduper.Begin.
type DedupeStatus int

const (
	// StatusNew means the id has not been seen: process the event, then
	// call Commit on success or Release on failure.
	StatusNew DedupeStatus = iota
	// StatusDuplicate means the event was already processed successfully.
	StatusDuplicate
	// StatusInFlight means another delivery of the same event is being
	// processed right now. Its outcome is unknown, so the caller must not
	// report success.
	StatusInFlight
)

func (s DedupeStatus) String() string {
	switch s {
	case StatusNew:
		return "new"
	case StatusDuplicate:
		return "duplicate"
	case StatusInFlight:
		return "in_flight"
	}
	return "unknown"
}

// DeduperConfig configures a Deduper. Zero fields take the defaults.
type DeduperConfig struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

// Deduper remembers which event ids have been processed so that
// at-least-once delivery becomes effectively-once processing.
//
// # Correctness under at-least-once delivery
//
// Clearinghouse retries a delivery until it gets a 2xx, for about three
// simulated days. A duplicate arrives when a 2xx is lost (sender crash,
// timeout) or when two retries race. Two rules follow:
//
//  1. An id is recorded as processed only after the callback succeeds
//     (Begin, then Commit). If processing fails, Release forgets the id so
//     the sender's retry is processed rather than acknowledged and lost.
//  2. While one delivery of an id is in flight, a concurrent delivery gets
//     StatusInFlight and must not be acknowledged, because the first may
//     still fail.
//
// # Bounded memory, and what it costs
//
// Entries expire after TTL and the oldest entries are evicted beyond
// MaxEntries. Both can only make the Deduper forget, never invent, an id:
// an evicted id delivered again is processed twice, but a new event is
// never dropped. That is the right direction to fail for fraud labels (a
// lost dispute is a missed fraud label; a doubled one is harmless if the
// label store upserts by dispute id, which it must anyway because a manual
// replay can arrive after any TTL). Choose TTL at least as long as the
// sender's retry window, and watch Evictions: a non-zero count means
// capacity, not TTL, is deciding what is forgotten.
//
// A Deduper is safe for concurrent use. One mutex guards it; webhook
// volume is far below the rate where that matters.
type Deduper struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu        sync.Mutex
	byID      map[string]*list.Element // value is *seenEntry
	order     *list.List               // oldest first by seen time
	evictions uint64
}

type seenEntry struct {
	id   string
	seen time.Time
	done bool
}

// NewDeduper returns an empty Deduper.
func NewDeduper(cfg DeduperConfig) *Deduper {
	d := &Deduper{
		ttl:        cfg.TTL,
		maxEntries: cfg.MaxEntries,
		now:        cfg.Now,
		byID:       make(map[string]*list.Element),
		order:      list.New(),
	}
	if d.ttl <= 0 {
		d.ttl = DefaultDedupeTTL
	}
	if d.maxEntries <= 0 {
		d.maxEntries = DefaultDedupeMaxEntries
	}
	if d.now == nil {
		d.now = time.Now
	}
	return d
}

// Begin claims id for processing. See DedupeStatus for what to do next.
func (d *Deduper) Begin(id string) DedupeStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.expire(now)
	if el, ok := d.byID[id]; ok {
		if el.Value.(*seenEntry).done {
			return StatusDuplicate
		}
		return StatusInFlight
	}
	d.insert(&seenEntry{id: id, seen: now})
	return StatusNew
}

// Commit records id as processed. The TTL runs from the commit time.
func (d *Deduper) Commit(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if el, ok := d.byID[id]; ok {
		e := el.Value.(*seenEntry)
		e.done, e.seen = true, now
		d.order.MoveToBack(el)
		return
	}
	// The claim was evicted while processing; record the result anyway.
	d.insert(&seenEntry{id: id, seen: now, done: true})
}

// Release abandons a claim made by Begin so a retry is processed.
func (d *Deduper) Release(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.byID[id]; ok && !el.Value.(*seenEntry).done {
		d.remove(el)
	}
}

// Len returns the number of ids currently remembered, in flight or done.
func (d *Deduper) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order.Len()
}

// Evictions returns how many ids were forgotten because of MaxEntries
// before their TTL ran out.
func (d *Deduper) Evictions() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evictions
}

// SeenEvent is one processed id in a snapshot.
type SeenEvent struct {
	ID   string    `json:"id"`
	Seen time.Time `json:"seen"`
}

// Snapshot returns the processed ids, oldest first. In-flight claims are
// left out: their events were not processed, so after a restart their
// retries must be.
func (d *Deduper) Snapshot() []SeenEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(d.now())
	out := make([]SeenEvent, 0, d.order.Len())
	for el := d.order.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*seenEntry); e.done {
			out = append(out, SeenEvent{ID: e.id, Seen: e.seen})
		}
	}
	return out
}

// Restore merges a snapshot into d, typically on an empty Deduper at
// startup. Expired entries are dropped and MaxEntries still applies.
func (d *Deduper) Restore(events []SeenEvent) {
	sorted := slices.Clone(events)
	slices.SortStableFunc(sorted, func(a, b SeenEvent) int { return a.Seen.Compare(b.Seen) })

	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for _, ev := range sorted {
		if now.Sub(ev.Seen) >= d.ttl {
			continue
		}
		if el, ok := d.byID[ev.ID]; ok {
			d.remove(el)
		}
		d.insert(&seenEntry{id: ev.ID, seen: ev.Seen, done: true})
	}
	// Merging into a non-empty Deduper can leave the list out of time
	// order; restore the invariant expire relies on.
	d.resort()
	d.expire(now)
}

// insert appends e as the newest entry, evicting the oldest past capacity.
func (d *Deduper) insert(e *seenEntry) {
	d.byID[e.id] = d.order.PushBack(e)
	for d.order.Len() > d.maxEntries {
		d.remove(d.order.Front())
		d.evictions++
	}
}

// expire drops entries older than the TTL. The list is ordered by seen
// time, so it stops at the first live entry: amortized O(1) per insert.
func (d *Deduper) expire(now time.Time) {
	for el := d.order.Front(); el != nil; el = d.order.Front() {
		if now.Sub(el.Value.(*seenEntry).seen) < d.ttl {
			return
		}
		d.remove(el)
	}
}

func (d *Deduper) remove(el *list.Element) {
	delete(d.byID, el.Value.(*seenEntry).id)
	d.order.Remove(el)
}

func (d *Deduper) resort() {
	entries := make([]*seenEntry, 0, d.order.Len())
	for el := d.order.Front(); el != nil; el = el.Next() {
		entries = append(entries, el.Value.(*seenEntry))
	}
	if slices.IsSortedFunc(entries, cmpSeen) {
		return
	}
	slices.SortStableFunc(entries, cmpSeen)
	d.order.Init()
	for _, e := range entries {
		d.byID[e.id] = d.order.PushBack(e)
	}
}

func cmpSeen(a, b *seenEntry) int { return a.seen.Compare(b.seen) }
