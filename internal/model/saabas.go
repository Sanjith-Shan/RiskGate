package model

import "fmt"

// Contributions computes Saabas feature contributions for x and writes them
// to out, which must have NumFeatures entries; it overwrites out and returns
// the bias. bias + sum(out) equals PredictRaw(x) up to float64 rounding.
// It is PredictContributions without the raw score.
//
// The Saabas method (from Ando Saabas's treeinterpreter) follows the path x
// takes through each tree. It starts at the root's value, and every time the
// path moves from a node to a child it credits the change in node value to
// the feature the node split on. A tree's contributions therefore telescope
// to its leaf output minus its root value, and the root values of all trees
// summed are the bias. Node values are LightGBM's internal_value, the output
// that node would have had as a leaf; the text format stores them with six
// significant digits, which moves bias and contributions by about 1e-6
// relative but does not break the sum, since each step is child minus parent.
//
// This is not SHAP. TreeSHAP averages a feature's marginal effect over every
// order in which features could be revealed, which makes it consistent (a
// model that relies more on a feature never gives it less credit) and fair to
// features that interact. Saabas credits only the one order the tree happens
// to test features in, so it is path-dependent and biased toward features
// split near the root. RiskGate uses Saabas because it is exact to compute in
// one pass over the path, costs about what a prediction costs, and is good
// enough to answer "which features pushed this payment's score up", which is
// all a reason string needs. LightGBM's own pred_contrib output is TreeSHAP
// and will not match these numbers.
//
// Contributions does not allocate.
func (m *Model) Contributions(x, out []float64) (bias float64) {
	_, bias = m.PredictContributions(x, out)
	return bias
}

// PredictContributions is PredictRaw and Contributions in one walk of each
// tree, for a caller that needs both, such as the service, which logs the
// contributions of every payment it scores. raw has exactly PredictRaw's
// bits: leaf outputs are added to a float64 that starts at zero, in tree
// order, and nothing else is added to it. out and bias are exactly what
// Contributions gives. It does not allocate.
func (m *Model) PredictContributions(x, out []float64) (raw, bias float64) {
	if len(x) != len(m.features) || len(out) != len(m.features) {
		panic(fmt.Sprintf("model: PredictContributions needs %d inputs and outputs, got %d and %d", len(m.features), len(x), len(out)))
	}
	clear(out)
	for i := range m.trees {
		t := &m.trees[i]
		if len(t.nodes) == 0 {
			raw += t.leaves[0]
			bias += t.leaves[0]
			continue
		}
		bias += t.internal[0]
		n := int32(0)
		for {
			nd := &t.nodes[n]
			next := t.next(nd, x[nd.feature])
			if next < 0 {
				leaf := t.leaves[^next]
				out[nd.feature] += leaf - t.internal[n]
				raw += leaf
				break
			}
			out[nd.feature] += t.internal[next] - t.internal[n]
			n = next
		}
	}
	return raw, bias
}
