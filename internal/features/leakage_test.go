package features

import (
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// TestLeakage is the point-in-time check. For each sampled transaction i it
// rewrites history after i (mutating or deleting every later event and
// inserting new ones, including payments at the same second with a larger
// TransactionID and on the same card and device), shuffles the whole input,
// runs the same pipeline the export runs (data.Sort, then Replay), and
// requires the features of i and of everything before it to be
// bit-identical to the untouched run.
//
// Knobs, so CI stays fast and a full run is one command away:
//
//	RISKGATE_LEAKAGE_SAMPLES  sampled transactions (default 48)
//	RISKGATE_LEAKAGE_ROWS     synthetic rows when no data file (default 20000)
//	RISKGATE_LEAKAGE_DATA     a data cache (data/cache/*.rgc) to use instead
//
// For example, on the full synthetic set:
//
//	RISKGATE_LEAKAGE_DATA=$PWD/data/cache/synth.rgc RISKGATE_LEAKAGE_SAMPLES=2000 \
//	  go test -run TestLeakage -timeout 1h ./internal/features/
func TestLeakage(t *testing.T) {
	samples := envInt(t, "RISKGATE_LEAKAGE_SAMPLES", 48)
	rows := envInt(t, "RISKGATE_LEAKAGE_ROWS", 20000)
	if testing.Short() || raceEnabled {
		samples = min(samples, 12)
	}
	var txns []data.Txn
	if path := os.Getenv("RISKGATE_LEAKAGE_DATA"); path != "" {
		var err error
		if txns, _, err = data.ReadCacheFile(path); err != nil {
			t.Fatal(err)
		}
	} else {
		txns = synthetic(t, rows, 20)
	}

	kinds := []func() State{
		func() State { return NewExact(0) },
		func() State { return NewBucketed(BucketedConfig{}) },
		func() State { return NewSketch(SketchConfig{Width: 1 << 12, HLLWidth: 256}) },
	}
	refs := make([][]uint64, len(kinds))
	for i, k := range kinds {
		var err error
		if refs[i], err = rowBits(k(), txns, len(txns)); err != nil {
			t.Fatal(err)
		}
	}

	r := rand.New(rand.NewPCG(2026, 9))
	picks := make([]int, samples)
	for i := range picks {
		picks[i] = r.IntN(len(txns))
	}
	var next, checked atomic.Int64
	var wg sync.WaitGroup
	for w := range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 77))
			for {
				s := int(next.Add(1) - 1)
				if s >= len(picks) || t.Failed() {
					return
				}
				i, kind := picks[s], s%len(kinds)
				mutated := rewriteFuture(r, txns, i)
				got, err := rowBits(kinds[kind](), mutated, i+1)
				if err != nil {
					t.Error(err) // not Fatal: this is not the test goroutine
					return
				}
				width := cat.NumCount()
				for j := 0; j <= i; j++ {
					for f := range width {
						if got[j*width+f] != refs[kind][j*width+f] {
							t.Errorf("sample %d (state %d): txn %d (ID %d) %s changed when the future after txn %d changed",
								s, kind, j, txns[j].ID, numName(f), i)
							return
						}
					}
				}
				checked.Add(int64(i + 1))
			}
		})
	}
	wg.Wait()
	t.Logf("%d samples, %d rows compared bit for bit, %d transactions", samples, checked.Load(), len(txns))
}

// rewriteFuture copies txns, keeps 0..i, and rewrites everything after i.
// It returns the result shuffled.
func rewriteFuture(r *rand.Rand, txns []data.Txn, i int) []data.Txn {
	out := make([]data.Txn, 0, len(txns)+16)
	out = append(out, txns[:i+1]...)
	pivot := txns[i]
	for j := i + 1; j < len(txns); j++ {
		t := txns[j]
		switch r.IntN(4) {
		case 0:
			continue // deleted
		case 1:
			t.Amount = math.Round(r.Float64()*1e5) / 100
			t.Card1, t.DeviceInfo, t.PEmail = pivot.Card1, pivot.DeviceInfo, pivot.PEmail
		case 2:
			t.DT += r.Int64N(7200) // later still, never earlier
			t.D1 = float64(r.IntN(100))
			t.Addr1 = pivot.Addr1
		}
		t.IsFraud = 1 - t.IsFraud
		out = append(out, t)
	}
	// New payments at the pivot's second, ordered after it by ID, and just
	// after it, on the pivot's own keys.
	maxID := txns[len(txns)-1].ID
	for _, t := range txns {
		maxID = max(maxID, t.ID)
	}
	for k := range 8 {
		t := pivot
		t.ID = maxID + 1 + int64(k)
		t.DT = pivot.DT + int64(k/2)*r.Int64N(60)
		t.Amount = 999.999
		t.Card1 = pivot.Card1 + float64(k%2)
		out = append(out, t)
	}
	r.Shuffle(len(out), func(a, b int) { out[a], out[b] = out[b], out[a] })
	return out
}

// rowBits runs the export's pipeline (sort the whole input, check it,
// replay) and returns the numeric features' bits of the first n rows,
// row-major. The replay stops after row n: rows after it cannot change
// earlier ones in a forward replay, and the ordering, where a leak would
// come from, is decided by the full sort beforehand.
func rowBits(st State, txns []data.Txn, n int) ([]uint64, error) {
	txns = append([]data.Txn(nil), txns...)
	data.Sort(txns)
	if err := data.CheckSorted(txns); err != nil {
		return nil, err
	}
	e, err := NewEngine(cat, st)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, n*cat.NumCount())
	err = Replay(e, txns[:n], func(_ *data.Txn, row schema.Row) error {
		for _, v := range row.Num {
			out = append(out, math.Float64bits(v))
		}
		return nil
	})
	return out, err
}

func numName(slot int) string {
	for _, f := range cat.Fields() {
		if f.Kind == schema.Number && f.Slot == slot {
			return f.Name
		}
	}
	return "?"
}

func envInt(t testing.TB, name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", name, s)
	}
	return n
}
