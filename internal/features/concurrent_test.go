package features

import (
	"bytes"
	"math/rand/v2"
	"sync"
	"testing"
)

var concurrentKinds = []struct {
	name string
	new  func() State
}{
	{"sharded-16", func() State { return NewSharded(16, func() State { return NewExact(0) }) }},
	{"locked", func() State { return NewLocked(func() State { return NewExact(0) }) }},
	{"syncmap", func() State { return NewSyncMap(NewExact(0)) }},
	{"sharded-bucketed", func() State { return NewSharded(8, func() State { return NewBucketed(BucketedConfig{}) }) }},
}

// Goroutines that own disjoint keys must leave the same state as one
// goroutine doing all the work: concurrency may interleave keys, never
// corrupt one.
func TestConcurrentDisjointKeysMatchSequential(t *testing.T) {
	const workers, keysPer, events = 8, 50, 400
	type op struct {
		k  Key
		ev Event
	}
	plans := make([][]op, workers)
	for w := range plans {
		r := rand.New(rand.NewPCG(uint64(w), 9))
		tm := int64(0)
		for range events {
			tm += r.Int64N(300)
			k, _ := CardKey(float64(w*keysPer + r.IntN(keysPer)))
			if r.IntN(3) == 0 {
				k, _ = DeviceKey(string(rune('a' + w)))
			}
			plans[w] = append(plans[w], op{k, Event{Time: tm, Milli: r.Int64N(100000), Card: uint64(r.IntN(20))}})
		}
	}
	for _, kind := range concurrentKinds {
		seq, par := kind.new(), kind.new()
		for _, p := range plans {
			for _, o := range p {
				seq.ReadAdd(o.k, o.ev)
			}
		}
		var wg sync.WaitGroup
		for _, p := range plans {
			wg.Go(func() {
				for _, o := range p {
					par.ReadAdd(o.k, o.ev)
					par.Read(o.k, o.ev.Time)
				}
			})
		}
		wg.Wait()
		if !bytes.Equal(snapshot(t, seq), snapshot(t, par)) {
			t.Errorf("%s: concurrent result differs from sequential", kind.name)
		}
	}
}

// Many goroutines on shared keys, with eviction running: run under -race.
// Every payment must be counted exactly once.
func TestConcurrentSharedKeysCountEverything(t *testing.T) {
	txns := synthetic(t, 20000, 3) // three days: every event stays inside 7d
	for _, kind := range concurrentKinds {
		e := newEngine(t, kind.new())
		e.SetEvictEvery(1000)
		var wg sync.WaitGroup
		const workers = 8
		for w := range workers {
			wg.Go(func() {
				row := e.NewRow()
				for i := w; i < len(txns); i += workers {
					e.ScoreAndUpdate(&txns[i], row)
				}
			})
		}
		wg.Wait()
		want := map[Key]float64{}
		for i := range txns {
			if k, ok := CardKey(txns[i].Card1); ok {
				want[k]++
			}
		}
		end := txns[len(txns)-1].DT + 1
		for k, n := range want {
			if got := e.State().Read(k, end).Count[weekWindow]; got != n {
				t.Fatalf("%s: %v has %v payments in 7d, want %v", kind.name, k, got, n)
			}
		}
	}
}
