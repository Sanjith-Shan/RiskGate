package model

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// saabasReference is the two-pass Saabas walk as it was before
// PredictContributions fused it with the prediction: the reference the fused
// walk must reproduce exactly.
func saabasReference(m *Model, x, out []float64) (bias float64) {
	clear(out)
	for i := range m.trees {
		t := &m.trees[i]
		if len(t.nodes) == 0 {
			bias += t.leaves[0]
			continue
		}
		bias += t.internal[0]
		n := int32(0)
		for n >= 0 {
			nd := &t.nodes[n]
			next := t.next(nd, x[nd.feature])
			var v float64
			if next >= 0 {
				v = t.internal[next]
			} else {
				v = t.leaves[^next]
			}
			out[nd.feature] += v - t.internal[n]
			n = next
		}
	}
	return bias
}

// randomInputs returns n input vectors built from the fixture's own values
// (which sit on and beside every split threshold) mixed column by column with
// NaN, signed zeros, values inside the zero threshold, infinities and, for
// categorical splits, negative, fractional and out-of-bitset codes.
func randomInputs(rows [][]float64, n int, seed uint64) [][]float64 {
	r := rand.New(rand.NewPCG(seed, 7))
	special := []float64{
		math.NaN(), 0, math.Copysign(0, -1), 1e-36, -1e-36, zeroThreshold,
		math.Inf(1), math.Inf(-1), -1, -0.5, 0.5, 3, 31, 32, 33, 64, 1e9, -1e9,
	}
	out := make([][]float64, n)
	for i := range out {
		x := make([]float64, len(rows[0]))
		for j := range x {
			switch r.IntN(4) {
			case 0:
				x[j] = special[r.IntN(len(special))]
			case 1:
				x[j] = float64(r.IntN(200) - 20)
			default:
				x[j] = rows[r.IntN(len(rows))][j]
			}
		}
		out[i] = x
	}
	return out
}

// TestPredictContributionsExact checks the fused walk against the two it
// replaces: raw bit for bit against PredictRaw (and so against LightGBM on
// the fixture rows), bias and every contribution bit for bit against the
// separate Saabas walk, on the fixture rows and on random inputs.
func TestPredictContributionsExact(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			m, rows, want := loadFixture(t, name)
			inputs := append(slices.Clone(rows), randomInputs(rows, 3000, uint64(len(name)))...)
			out := make([]float64, m.NumFeatures())
			ref := make([]float64, m.NumFeatures())
			for i, x := range inputs {
				raw, bias := m.PredictContributions(x, out)
				if got, w := math.Float64bits(raw), math.Float64bits(m.PredictRaw(x)); got != w {
					t.Fatalf("input %d: fused raw %v, PredictRaw %v", i, raw, m.PredictRaw(x))
				}
				if i < len(rows) && math.Float64bits(raw) != math.Float64bits(want[i]) {
					t.Fatalf("row %d: fused raw %v, LightGBM %v", i, raw, want[i])
				}
				refBias := saabasReference(m, x, ref)
				if math.Float64bits(bias) != math.Float64bits(refBias) {
					t.Fatalf("input %d: fused bias %v, reference %v", i, bias, refBias)
				}
				for j := range out {
					if math.Float64bits(out[j]) != math.Float64bits(ref[j]) {
						t.Fatalf("input %d feature %d: fused %v, reference %v", i, j, out[j], ref[j])
					}
				}
				if b := m.Contributions(x, ref); math.Float64bits(b) != math.Float64bits(bias) || !slices.Equal(ref, out) {
					t.Fatalf("input %d: Contributions differs from PredictContributions", i)
				}
				sum := bias
				for _, c := range out {
					sum += c
				}
				if math.Abs(sum-raw) > 1e-9 {
					t.Fatalf("input %d: bias+contributions = %v, raw = %v", i, sum, raw)
				}
			}
			t.Logf("%d inputs (%d fixture rows), all bit-identical", len(inputs), len(rows))
		})
	}
}

func TestScoreContributionsMatchesScore(t *testing.T) {
	s, c, rows, _ := largeScorer(t)
	n := s.NumFeatures()
	x, contrib := make([]float64, n), make([]float64, n)
	x2, contrib2 := make([]float64, n), make([]float64, n)
	checked := 0
	for _, in := range append(rows[:500:500], randomInputs(rows, 500, 1)...) {
		r, ok := toRow(c, s.FeatureNames(), in)
		if !ok {
			continue
		}
		checked++
		score, prob, raw := s.Score(r)
		score2, prob2, raw2, bias2 := s.ScoreContributions(r, x2, contrib2)
		bias := s.Contributions(r, x, contrib)
		if score != score2 || math.Float64bits(prob) != math.Float64bits(prob2) || math.Float64bits(raw) != math.Float64bits(raw2) ||
			math.Float64bits(bias) != math.Float64bits(bias2) || !slices.Equal(contrib, contrib2) {
			t.Fatalf("ScoreContributions (%d %v %v %v) differs from Score and Contributions (%d %v %v %v)",
				score2, prob2, raw2, bias2, score, prob, raw, bias)
		}
	}
	if checked < 300 {
		t.Fatalf("only %d rows checked", checked)
	}
	r, _ := toRow(c, s.FeatureNames(), rows[0])
	if a := testing.AllocsPerRun(20, func() { s.ScoreContributions(r, x, contrib) }); a != 0 {
		t.Errorf("ScoreContributions allocates %v times", a)
	}
}

// TestTopPositive checks the partial selection against sorting everything,
// on values with many ties, zeros and negatives.
func TestTopPositive(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 2000 {
		c := make([]float64, r.IntN(60))
		for i := range c {
			c[i] = float64(r.IntN(9)-3) / 2
		}
		if len(c) > 0 && r.IntN(5) == 0 {
			c[r.IntN(len(c))] = math.NaN()
		}
		var want []int
		for i, v := range c {
			if v > 0 {
				want = append(want, i)
			}
		}
		slices.SortFunc(want, func(a, b int) int { return cmp.Or(cmp.Compare(c[b], c[a]), cmp.Compare(a, b)) })
		for limit := range 6 {
			w := want[:min(limit, len(want))]
			if got := topPositive(c, limit, nil); !slices.Equal(got, w) && len(got)+len(w) > 0 {
				t.Fatalf("limit %d, %v: got %v, want %v", limit, c, got, w)
			}
		}
	}
}

func BenchmarkPredictContributions(b *testing.B) {
	m, rows, _ := loadFixture(b, "large")
	out := make([]float64, m.NumFeatures())
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		m.PredictContributions(rows[i], out)
		if i++; i == len(rows) {
			i = 0
		}
	}
}
