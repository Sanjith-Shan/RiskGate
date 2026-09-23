package model

import (
	"fmt"
	"math"
)

// PredictRaw returns the model's raw score (log-odds) for x, which must hold
// NumFeatures values in FeatureNames order, NaN for missing. It is
// bit-identical to LightGBM's Booster.predict(x, raw_score=True): the trees
// are summed in file order into a float64 that starts at zero, which is what
// GBDT::PredictRaw does, and every split follows tree.h exactly. It does not
// allocate.
func (m *Model) PredictRaw(x []float64) float64 {
	if len(x) != len(m.features) {
		panic(fmt.Sprintf("model: input has %d values, model has %d features", len(x), len(m.features)))
	}
	var sum float64
	for i := range m.trees {
		t := &m.trees[i]
		sum += t.leaves[t.leaf(x)]
	}
	return sum
}

// Probability is the uncalibrated probability LightGBM's predict returns
// without raw_score: 1 / (1 + exp(-sigmoid * raw)). RiskGate's risk_score does
// not use it (see Calibrator); it is here for reporting.
func (m *Model) Probability(raw float64) float64 {
	return 1 / (1 + math.Exp(-m.sigmoid*raw))
}

// leaf walks t for x and returns the index of the leaf reached.
func (t *tree) leaf(x []float64) int {
	if len(t.nodes) == 0 {
		return 0
	}
	n := int32(0)
	for n >= 0 {
		nd := &t.nodes[n]
		n = t.next(nd, x[nd.feature])
	}
	return int(^n)
}

// next is Tree::Decision from LightGBM's tree.h.
func (t *tree) next(nd *node, v float64) int32 {
	if nd.categorical {
		return t.categorical(nd, v)
	}
	// The predictor stores only values with |v| > kZeroThreshold (or NaN) in
	// its feature buffer; anything smaller reaches the tree as exactly 0.
	if math.Abs(v) <= zeroThreshold {
		v = 0
	}
	if v != v { // NaN
		if nd.missing == missingNaN {
			return nd.dflt()
		}
		v = 0 // with missing type None or Zero, NaN is converted to 0
	}
	if nd.missing == missingZero && v == 0 {
		return nd.dflt()
	}
	if v <= nd.threshold {
		return nd.left
	}
	return nd.right
}

func (nd *node) dflt() int32 {
	if nd.defaultLeft {
		return nd.left
	}
	return nd.right
}

// categorical is Tree::CategoricalDecision. The value is truncated toward
// zero to an int category (static_cast<int>), NaN and negative categories go
// right, and so does any category outside the bitset: unseen categories and
// values too large for an int all fail the bitset lookup.
func (t *tree) categorical(nd *node, v float64) int32 {
	if v != v || v <= -1 {
		return nd.right
	}
	words := t.catWords[nd.catLo:nd.catHi]
	if v >= float64(len(words)*32) {
		return nd.right
	}
	c := int(v) // 0 <= c < 32*len(words); -1 < v < 0 truncates to 0, as in C++
	if words[c>>5]>>(c&31)&1 != 0 {
		return nd.left
	}
	return nd.right
}
