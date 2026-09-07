package backtest

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

// TestNumSetMatchesSearch checks the hashed membership test against binary
// search on lists of every size the kernels switch between, including the
// values where float equality is subtle: both zeros, NaN, infinities,
// subnormals and integers past 2^53.
func TestNumSetMatchesSearch(t *testing.T) {
	edge := []float64{0, math.Copysign(0, -1), math.NaN(), math.Inf(1), math.Inf(-1), 5e-324, -5e-324,
		math.MaxFloat64, -math.MaxFloat64, 1 << 53, 1<<53 + 2, 9007199254740993, 1, -1, 0.1, 49.99}
	rng := rand.New(rand.NewPCG(3, 4))
	for size := range 200 {
		var vals []float64
		for range size {
			switch rng.IntN(3) {
			case 0:
				vals = append(vals, edge[rng.IntN(len(edge))])
			case 1:
				vals = append(vals, float64(rng.IntN(600))) // billing_region-like codes
			default:
				vals = append(vals, rng.NormFloat64()*1e3)
			}
		}
		set := rules.NumberList(vals...).Numbers // sorted, deduplicated, NaN removed
		h := newNumSet(set)
		if len(set) > 0 && h == nil {
			t.Fatalf("no hash table for %d values", len(set))
		}
		probes := append(slices.Clone(edge), vals...)
		for range 200 {
			probes = append(probes, float64(rng.IntN(600)), rng.NormFloat64()*1e3)
		}
		for _, v := range probes {
			want := containsNum(set, v)
			if h != nil && h.has(v) != want {
				t.Fatalf("set of %d: has(%v) = %v, want %v", len(set), v, h.has(v), want)
			}
		}
	}
}
