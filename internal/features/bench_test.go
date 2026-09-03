package features

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

// Benchmarks for experiment 4 (state shootout) and experiment 5 (concurrency).
// They run on SYNTHETIC data; numbers from them describe the implementation,
// not fraud.
//
//	go test -run '^$' -bench . -benchmem ./internal/features/

var benchKinds = []struct {
	name string
	new  func() State
}{
	{"exact", func() State { return NewExact(0) }},
	{"bucketed", func() State { return NewBucketed(BucketedConfig{}) }},
	{"sketch", func() State { return NewSketch(SketchConfig{}) }},
}

const benchRows, benchDays = 200000, 60

type keyedEvent struct {
	k  Key
	ev Event
}

func benchEvents(b *testing.B) []keyedEvent {
	txns := synthetic(b, benchRows, benchDays)
	var out []keyedEvent
	for i := range txns {
		keys, ok := KeysOf(&txns[i])
		for e := range Entity(NumEntities) {
			if ok[e] {
				out = append(out, keyedEvent{keys[e], EventOf(&txns[i])})
			}
		}
	}
	return out
}

// BenchmarkReplay is the end-to-end cost per payment: four keys read and
// updated, the row filled.
func BenchmarkReplay(b *testing.B) {
	txns := synthetic(b, benchRows, benchDays)
	for _, k := range benchKinds {
		b.Run(k.name, func(b *testing.B) {
			for b.Loop() {
				e := newEngine(b, k.new())
				if err := Replay(e, txns, nil); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(txns)), "ns/txn")
		})
	}
}

// BenchmarkScore is one read-only scoring against a warm state.
func BenchmarkScore(b *testing.B) {
	txns := synthetic(b, benchRows, benchDays)
	for _, k := range benchKinds {
		b.Run(k.name, func(b *testing.B) {
			e := newEngine(b, k.new())
			if err := Replay(e, txns, nil); err != nil {
				b.Fatal(err)
			}
			row := e.NewRow()
			tail := txns[len(txns)-20000:]
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				e.ScoreInto(&tail[i%len(tail)], row)
				i++
			}
		})
	}
}

// BenchmarkStateAdd and BenchmarkStateRead time the state alone, per key
// operation (ns/update, ns/read).
func BenchmarkStateAdd(b *testing.B) {
	evs := benchEvents(b)
	for _, k := range benchKinds {
		b.Run(k.name, func(b *testing.B) {
			st := k.new()
			i := 0
			for b.Loop() {
				if i == len(evs) {
					b.StopTimer()
					st, i = k.new(), 0
					b.StartTimer()
				}
				st.Add(evs[i].k, evs[i].ev)
				i++
			}
		})
	}
}

func BenchmarkStateRead(b *testing.B) {
	evs := benchEvents(b)
	for _, k := range benchKinds {
		b.Run(k.name, func(b *testing.B) {
			st := k.new()
			for _, e := range evs {
				st.Add(e.k, e.ev)
			}
			now := evs[len(evs)-1].ev.Time
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				st.Read(evs[i%len(evs)].k, now)
				i += 7
			}
		})
	}
}

// BenchmarkBytesPerKey reports each state's memory after a replay: the
// analytic estimate from Stats, and the heap growth the runtime measured.
func BenchmarkBytesPerKey(b *testing.B) {
	txns := synthetic(b, benchRows, benchDays)
	for _, k := range benchKinds {
		b.Run(k.name, func(b *testing.B) {
			var st State
			var heap uint64
			for b.Loop() {
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				st = k.new()
				e := newEngine(b, st)
				e.SetEvictEvery(0)
				if err := Replay(e, txns, nil); err != nil {
					b.Fatal(err)
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				heap = after.HeapAlloc - min(after.HeapAlloc, before.HeapAlloc)
			}
			s := st.Stats()
			keys := s.Keys
			if keys < 0 { // a sketch cannot count its keys; use the exact count
				keys = exactStats(txns).Keys
			}
			b.ReportMetric(float64(s.Bytes)/float64(keys), "est-B/key")
			b.ReportMetric(float64(heap)/float64(keys), "heap-B/key")
			b.ReportMetric(float64(keys), "keys")
			runtime.KeepAlive(st)
		})
	}
}

// exactStats replays txns into an exact state and returns its stats.
func exactStats(txns []data.Txn) Stats {
	st := NewExact(0)
	for i := range txns {
		keys, ok := KeysOf(&txns[i])
		for e := range Entity(NumEntities) {
			if ok[e] {
				st.Add(keys[e], EventOf(&txns[i]))
			}
		}
	}
	return st.Stats()
}

// BenchmarkParallel is experiment 5's contention comparison: every
// goroutine does ReadAdd on keys drawn from the real key mix, with a shared
// clock moving forward.
func BenchmarkParallel(b *testing.B) {
	evs := benchEvents(b)
	kinds := []struct {
		name string
		new  func() State
	}{
		{"locked", func() State { return NewLocked(func() State { return NewExact(0) }) }},
		{"sharded-4", func() State { return NewSharded(4, func() State { return NewExact(0) }) }},
		{"sharded-16", func() State { return NewSharded(16, func() State { return NewExact(0) }) }},
		{"sharded-64", func() State { return NewSharded(64, func() State { return NewExact(0) }) }},
		{"syncmap", func() State { return NewSyncMap(NewExact(0)) }},
	}
	for _, k := range kinds {
		b.Run(fmt.Sprintf("%s/procs=%d", k.name, runtime.GOMAXPROCS(0)), func(b *testing.B) {
			st := k.new()
			var next atomic.Int64
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := next.Add(1)
					e := evs[int(i)%len(evs)]
					e.ev.Time = i // a single clock, so no key ever goes back in time
					st.ReadAdd(e.k, e.ev)
				}
			})
		})
	}
}
