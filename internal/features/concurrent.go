package features

import (
	"bytes"
	"fmt"
	"io"
	"math/bits"
	"sync"
	"unsafe"
)

// The online service reads and updates state from many request goroutines.
// Three ways to make a State safe for that, compared in experiment 5:
//
//   - NewSharded: keys hash to one of N states, each behind its own
//     read-write mutex. Requests for different shards never contend.
//   - NewLocked: one state behind one mutex, the baseline.
//   - NewSyncMap: a sync.Map of per-key entries, each with its own mutex.
//     Contention is per key, at the price of sync.Map's overhead and a
//     pointer chase per access.

// Sharded partitions keys across independently locked states.
type Sharded struct {
	shards   []shard
	newState func() State
}

// cacheLine is 128 bytes on Apple silicon, which is where the experiments
// run; padding shards to it keeps one shard's lock traffic from invalidating
// its neighbour's line.
const cacheLine = 128

type shard struct {
	mu sync.RWMutex
	st State
	_  [cacheLine - (unsafe.Sizeof(sync.RWMutex{})+16)%cacheLine]byte
}

// NewSharded returns a state of n shards, each made by newState. Sharding a
// Sketch gives every shard its own full-size sketch.
func NewSharded(n int, newState func() State) *Sharded {
	if n < 1 {
		panic("features: need at least one shard")
	}
	s := &Sharded{shards: make([]shard, n), newState: newState}
	for i := range s.shards {
		s.shards[i].st = newState()
	}
	return s
}

// NewLocked is a single state behind one global mutex.
func NewLocked(newState func() State) *Sharded { return NewSharded(1, newState) }

// Shards returns the shard count.
func (s *Sharded) Shards() int { return len(s.shards) }

// shardOf maps the key's hash onto [0, n) by multiply-shift, which is
// unbiased for any n, unlike taking the hash modulo n.
func (s *Sharded) shardOf(k Key) *shard {
	hi, _ := bits.Mul64(k.Hash(), uint64(len(s.shards)))
	return &s.shards[hi]
}

func (s *Sharded) Read(k Key, now int64) Aggregates {
	sh := s.shardOf(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.st.Read(k, now)
}

func (s *Sharded) Add(k Key, ev Event) {
	sh := s.shardOf(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.st.Add(k, ev)
}

func (s *Sharded) ReadAdd(k Key, ev Event) Aggregates {
	sh := s.shardOf(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.st.ReadAdd(k, ev)
}

// EvictIdle sweeps one shard at a time, so it never stops the world.
func (s *Sharded) EvictIdle(now int64) int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += sh.st.EvictIdle(now)
		sh.mu.Unlock()
	}
	return n
}

func (s *Sharded) Stats() Stats {
	var total Stats
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		st := sh.st.Stats()
		sh.mu.RUnlock()
		if st.Keys < 0 || total.Keys < 0 {
			total.Keys = -1
		} else {
			total.Keys += st.Keys
		}
		total.Bytes += st.Bytes
	}
	return total
}

const shardedMagic = "riskgate/sharded/v1"

// Snapshot writes each shard's snapshot in turn, length-prefixed. Shards are
// locked one at a time, so the result is consistent per shard; take it with
// traffic stopped (at shutdown) for a single point in time.
func (s *Sharded) Snapshot(w io.Writer) error {
	enc := newEncoder(w)
	enc.str(shardedMagic)
	enc.varint(int64(len(s.shards)))
	var buf bytes.Buffer
	for i := range s.shards {
		sh := &s.shards[i]
		buf.Reset()
		sh.mu.RLock()
		err := sh.st.Snapshot(&buf)
		sh.mu.RUnlock()
		if err != nil {
			return err
		}
		enc.uvarint(uint64(buf.Len()))
		enc.write(buf.Bytes())
	}
	return enc.flush()
}

// Restore reads a snapshot taken with the same shard count, building fresh
// shards first so that a bad snapshot leaves the current state untouched.
func (s *Sharded) Restore(r io.Reader) error {
	d := newDecoder(r)
	if magic := d.str(); d.err == nil && magic != shardedMagic {
		return fmt.Errorf("features: snapshot is of a %q state, want %q", magic, shardedMagic)
	}
	d.expectInt("shard count", int64(len(s.shards)))
	fresh := make([]State, len(s.shards))
	for i := range fresh {
		n := d.count(1 << 40)
		if d.err != nil {
			return d.err
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(d.r, b); err != nil {
			return errSnapshot
		}
		fresh[i] = s.newState()
		if err := fresh[i].Restore(bytes.NewReader(b)); err != nil {
			return fmt.Errorf("shard %d: %w", i, err)
		}
	}
	if err := d.end(); err != nil {
		return err
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		sh.st = fresh[i]
		sh.mu.Unlock()
	}
	return nil
}

// SyncMap keeps per-key entries in a sync.Map, each with its own mutex.
type SyncMap struct {
	spec kindSpec
	m    sync.Map // Key -> *syncEntry
}

type syncEntry struct {
	mu    sync.Mutex
	e     entry
	fresh bool // created, no event yet
	dead  bool // evicted; whoever holds it must look the key up again
}

// NewSyncMap stores entries of the same kind and configuration as template,
// which must come from NewExact or NewBucketed. Its snapshots are
// interchangeable with template's.
func NewSyncMap(template State) *SyncMap {
	k, ok := template.(*keyed)
	if !ok {
		panic(fmt.Sprintf("features: NewSyncMap needs a per-key state, got %T", template))
	}
	return &SyncMap{spec: k.kindSpec}
}

func (s *SyncMap) Read(k Key, now int64) Aggregates {
	v, ok := s.m.Load(k)
	if !ok {
		return unseen(k.Entity)
	}
	se := v.(*syncEntry)
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.dead || se.fresh || s.spec.isIdle(se.e, now) {
		return unseen(k.Entity)
	}
	return se.e.read(now)
}

func (s *SyncMap) Add(k Key, ev Event) { s.ReadAdd(k, ev) }

func (s *SyncMap) ReadAdd(k Key, ev Event) Aggregates {
	for {
		v, ok := s.m.Load(k)
		if !ok {
			v, _ = s.m.LoadOrStore(k, &syncEntry{e: s.spec.newEntry(k.Entity.tracksDistinct()), fresh: true})
		}
		se := v.(*syncEntry)
		se.mu.Lock()
		if se.dead {
			se.mu.Unlock()
			continue // evicted between Load and Lock; a later Load sees its replacement
		}
		var a Aggregates
		switch {
		case se.fresh:
			a = unseen(k.Entity)
		case s.spec.isIdle(se.e, ev.Time):
			a = unseen(k.Entity)
			se.e = s.spec.newEntry(k.Entity.tracksDistinct())
		default:
			a = se.e.read(ev.Time)
		}
		se.e.add(ev)
		se.fresh = false
		se.mu.Unlock()
		return a
	}
}

func (s *SyncMap) EvictIdle(now int64) int {
	n := 0
	s.m.Range(func(k, v any) bool {
		se := v.(*syncEntry)
		se.mu.Lock()
		if !se.fresh && s.spec.isIdle(se.e, now) {
			se.dead = true
			s.m.CompareAndDelete(k, v)
			n++
		}
		se.mu.Unlock()
		return true
	})
	return n
}

func (s *SyncMap) Stats() Stats {
	var st Stats
	s.m.Range(func(k, v any) bool {
		se := v.(*syncEntry)
		se.mu.Lock()
		if !se.dead && !se.fresh {
			st.Keys++
			// Rough: a sync.Map entry is a boxed key, an entry pointer, and
			// the syncEntry itself, on top of what a plain map slot costs.
			st.Bytes += mapSlotBytes + int64(unsafe.Sizeof(syncEntry{})) + 64 + int64(len(k.(Key).Str)) + se.e.bytes()
		}
		se.mu.Unlock()
		return true
	})
	return st
}

// Snapshot writes the same format as the map-based store of the same kind.
// Each key is locked while it is written; stop traffic for a point-in-time
// snapshot.
func (s *SyncMap) Snapshot(w io.Writer) error {
	entries := make(map[Key]*syncEntry)
	s.m.Range(func(k, v any) bool {
		entries[k.(Key)] = v.(*syncEntry)
		return true
	})
	keys := make([]Key, 0, len(entries))
	for k, se := range entries {
		se.mu.Lock()
		if !se.dead && !se.fresh {
			keys = append(keys, k)
		}
		se.mu.Unlock()
	}
	enc := newEncoder(w)
	encodeKeyed(enc, &s.spec, keys, func(k Key, enc *encoder) {
		se := entries[k]
		se.mu.Lock()
		se.e.encode(enc)
		se.mu.Unlock()
	})
	return enc.flush()
}

// Restore replaces the contents. It must not run concurrently with other
// calls.
func (s *SyncMap) Restore(r io.Reader) error {
	d := newDecoder(r)
	m := make(map[Key]entry)
	decodeKeyed(d, &s.spec, func(k Key, e entry) { m[k] = e })
	if err := d.end(); err != nil {
		return err
	}
	s.m.Clear()
	for k, e := range m {
		s.m.Store(k, &syncEntry{e: e})
	}
	return nil
}
