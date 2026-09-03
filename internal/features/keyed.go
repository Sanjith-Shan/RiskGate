package features

import (
	"fmt"
	"io"
	"slices"
	"unsafe"
)

// entry is one key's history inside a per-key store (Exact or Bucketed).
type entry interface {
	// read returns the aggregates at now for a live key.
	read(now int64) Aggregates
	add(ev Event)
	lastSeen() int64
	bytes() int64
	encode(e *encoder)
	decode(d *decoder)
}

// kindSpec describes a per-key store kind: its snapshot magic, the
// configuration a snapshot must match, and how to make an empty entry.
type kindSpec struct {
	magic    string
	config   []int64
	ttl      int64
	newEntry func(distinct bool) entry
}

func (s *kindSpec) isIdle(e entry, now int64) bool { return now-e.lastSeen() >= s.ttl }

// keyed is a map from key to entry, with TTL handling and snapshots. Exact
// and Bucketed differ only in the entry.
type keyed struct {
	kindSpec
	m map[Key]entry
}

func newKeyed(spec kindSpec) *keyed { return &keyed{kindSpec: spec, m: make(map[Key]entry)} }

func (s *keyed) Read(k Key, now int64) Aggregates {
	if e, ok := s.m[k]; ok && !s.isIdle(e, now) {
		return e.read(now)
	}
	return unseen(k.Entity)
}

func (s *keyed) Add(k Key, ev Event) { s.ReadAdd(k, ev) }

func (s *keyed) ReadAdd(k Key, ev Event) Aggregates {
	e, ok := s.m[k]
	var a Aggregates
	if ok && !s.isIdle(e, ev.Time) {
		a = e.read(ev.Time)
	} else {
		a = unseen(k.Entity)
		e = s.newEntry(k.Entity.tracksDistinct())
		s.m[k] = e
	}
	e.add(ev)
	return a
}

func (s *keyed) EvictIdle(now int64) int {
	n := 0
	for k, e := range s.m {
		if s.isIdle(e, now) {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// mapSlotBytes estimates one map slot: the key, the interface value, and
// Go's swiss-table control byte, scaled for a 7/8 maximum load factor.
const mapSlotBytes = (int64(unsafe.Sizeof(Key{})) + 16 + 1) * 8 / 7

// Stats estimates memory from struct sizes and capacities. It is a model,
// not a heap profile; BenchmarkBytesPerKey measures the real heap.
func (s *keyed) Stats() Stats {
	st := Stats{Keys: len(s.m)}
	for k, e := range s.m {
		st.Bytes += mapSlotBytes + int64(len(k.Str)) + e.bytes()
	}
	return st
}

func (s *keyed) Snapshot(w io.Writer) error {
	enc := newEncoder(w)
	keys := make([]Key, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	encodeKeyed(enc, &s.kindSpec, keys, func(k Key, enc *encoder) { s.m[k].encode(enc) })
	return enc.flush()
}

func (s *keyed) Restore(r io.Reader) error {
	d := newDecoder(r)
	m := make(map[Key]entry)
	decodeKeyed(d, &s.kindSpec, func(k Key, e entry) { m[k] = e })
	if err := d.end(); err != nil {
		return err
	}
	s.m = m
	return nil
}

// encodeKeyed writes a per-key store. Keys are sorted, so equal stores give
// identical bytes whatever the map's iteration order. NewSyncMap shares the
// format, so a snapshot moves between it and the map-based stores.
func encodeKeyed(enc *encoder, spec *kindSpec, keys []Key, encodeEntry func(Key, *encoder)) {
	slices.SortFunc(keys, compareKeys)
	enc.str(spec.magic)
	enc.varint(spec.ttl)
	enc.uvarint(uint64(len(spec.config)))
	for _, c := range spec.config {
		enc.varint(c)
	}
	enc.uvarint(uint64(len(keys)))
	for _, k := range keys {
		enc.key(k)
		encodeEntry(k, enc)
	}
}

func decodeKeyed(d *decoder, spec *kindSpec, put func(Key, entry)) {
	if magic := d.str(); d.err == nil && magic != spec.magic {
		d.fail(fmt.Errorf("features: snapshot is of a %q state, this is a %q state", magic, spec.magic))
		return
	}
	d.expectInt("idle TTL", spec.ttl)
	if n := d.count(64); d.err == nil && n != len(spec.config) {
		d.fail(errSnapshot)
	}
	for i, c := range spec.config {
		d.expectInt(fmt.Sprintf("config[%d]", i), c)
	}
	n := d.count(1 << 40)
	for range n {
		if d.err != nil {
			return
		}
		k := d.key()
		e := spec.newEntry(k.Entity.tracksDistinct())
		e.decode(d)
		put(k, e)
	}
}
