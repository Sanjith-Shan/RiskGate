// Package model evaluates RiskGate's fraud model in Go. It reads the text
// file LightGBM writes with Booster.save_model, predicts raw scores that are
// bit-identical to LightGBM's own predict(raw_score=True), explains each score
// with Saabas contributions, and maps it through an isotonic calibration to
// the 0-99 risk_score that rules read.
//
// Python trains the model offline. Nothing in this package calls Python or C.
package model

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Missing-value handling of a numerical split, from bits 2-3 of LightGBM's
// decision_type (MissingType in LightGBM's tree.h).
const (
	missingNone = 0 // no missing values seen in training: NaN is treated as 0
	missingZero = 1 // zero_as_missing: 0 and NaN take the default direction
	missingNaN  = 2 // NaN takes the default direction
)

// decision_type bits (kCategoricalMask and kDefaultLeftMask in tree.h).
const (
	categoricalMask = 1
	defaultLeftMask = 2
)

// kZeroThreshold in LightGBM's meta.h. Values with |x| <= kZeroThreshold are
// zero as far as LightGBM is concerned. It is declared there as the float
// literal 1e-35f, so its double value is float32(1e-35), about
// 1.0000000180025095e-35, not 1e-35; the parity fixtures probe the difference.
const zeroThreshold = float64(float32(1e-35))

// node is one internal node. Children >= 0 are internal nodes; a negative
// child c is leaf ^c, the same encoding LightGBM uses.
type node struct {
	threshold   float64 // numerical: go left if x <= threshold
	feature     int32
	left, right int32
	categorical bool
	defaultLeft bool
	missing     uint8
	catLo       int32 // categorical: bitset is tree.catWords[catLo:catHi]
	catHi       int32
}

type tree struct {
	nodes    []node    // empty for a single-leaf tree
	leaves   []float64 // leaf outputs, shrinkage and any init score already applied
	internal []float64 // node values (internal_value), used only by Saabas
	catWords []uint32
}

// Model is a parsed LightGBM binary classifier. It is immutable after
// Parse and safe for concurrent use.
type Model struct {
	features []string
	sigmoid  float64
	trees    []tree
}

// Load parses a LightGBM text model file.
func Load(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Parse reads a model in LightGBM's text format (version v4, as written by
// Booster.save_model or model_to_string). Only the binary objective is
// supported; anything else is rejected rather than silently mis-scored.
func Parse(r io.Reader) (*Model, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<30)

	header := map[string]string{}
	var blocks []map[string]string
	var cur map[string]string
	ended := false
	for sc.Scan() {
		line := string(bytes.TrimSpace(sc.Bytes()))
		if line == "end of trees" {
			ended = true
			break
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Tree=") {
			want := strconv.Itoa(len(blocks))
			if line[len("Tree="):] != want {
				return nil, fmt.Errorf("lightgbm: found %q, want Tree=%s", line, want)
			}
			cur = map[string]string{}
			blocks = append(blocks, cur)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			if cur == nil && line == "tree" { // the file's first line
				continue
			}
			return nil, fmt.Errorf("lightgbm: unexpected line %q", truncate(line))
		}
		if cur == nil {
			header[k] = v
		} else {
			cur[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("lightgbm: %w", err)
	}
	if !ended {
		return nil, errors.New(`lightgbm: missing "end of trees"; file truncated?`)
	}

	m := &Model{}
	if err := m.parseHeader(header); err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("lightgbm: model has no trees")
	}
	m.trees = make([]tree, len(blocks))
	for i, b := range blocks {
		if err := parseTree(b, len(m.features), &m.trees[i]); err != nil {
			return nil, fmt.Errorf("lightgbm: Tree=%d: %w", i, err)
		}
	}
	return m, nil
}

func (m *Model) parseHeader(h map[string]string) error {
	if v := h["version"]; v != "v4" {
		return fmt.Errorf("lightgbm: model version %q, want v4", v)
	}
	if v := h["num_class"]; v != "1" {
		return fmt.Errorf("lightgbm: num_class=%s; only binary models are supported", v)
	}
	if v := h["num_tree_per_iteration"]; v != "1" {
		return fmt.Errorf("lightgbm: num_tree_per_iteration=%s; only binary models are supported", v)
	}
	if _, ok := h["average_output"]; ok {
		return errors.New("lightgbm: average_output (random forest mode) is not supported")
	}
	obj := strings.Fields(h["objective"])
	if len(obj) == 0 || obj[0] != "binary" {
		return fmt.Errorf("lightgbm: objective %q; only binary is supported", h["objective"])
	}
	m.sigmoid = 1
	for _, kv := range obj[1:] {
		if s, ok := strings.CutPrefix(kv, "sigmoid:"); ok {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil || v <= 0 {
				return fmt.Errorf("lightgbm: bad sigmoid in objective %q", h["objective"])
			}
			m.sigmoid = v
		}
	}
	m.features = strings.Fields(h["feature_names"])
	maxIdx, err := strconv.Atoi(h["max_feature_idx"])
	if err != nil {
		return fmt.Errorf("lightgbm: bad max_feature_idx %q", h["max_feature_idx"])
	}
	if len(m.features) != maxIdx+1 {
		return fmt.Errorf("lightgbm: %d feature_names but max_feature_idx=%d", len(m.features), maxIdx)
	}
	return nil
}

func parseTree(b map[string]string, nFeatures int, t *tree) error {
	numLeaves, err := strconv.Atoi(b["num_leaves"])
	if err != nil || numLeaves < 1 {
		return fmt.Errorf("bad num_leaves %q", b["num_leaves"])
	}
	if v := b["is_linear"]; v != "" && v != "0" {
		return errors.New("linear trees are not supported")
	}
	if t.leaves, err = floats(b, "leaf_value", numLeaves); err != nil {
		return err
	}
	if numLeaves == 1 {
		return nil // a constant tree: LightGBM returns leaf_value[0] without looking at x
	}
	n := numLeaves - 1
	feat, err := ints(b, "split_feature", n)
	if err != nil {
		return err
	}
	thr, err := floats(b, "threshold", n)
	if err != nil {
		return err
	}
	dt, err := ints(b, "decision_type", n)
	if err != nil {
		return err
	}
	left, err := ints(b, "left_child", n)
	if err != nil {
		return err
	}
	right, err := ints(b, "right_child", n)
	if err != nil {
		return err
	}
	if t.internal, err = floats(b, "internal_value", n); err != nil {
		return err
	}
	numCat, _ := strconv.Atoi(b["num_cat"])
	var bounds []int
	if numCat > 0 {
		if bounds, err = ints(b, "cat_boundaries", numCat+1); err != nil {
			return err
		}
		words := strings.Fields(b["cat_threshold"])
		if len(words) != bounds[numCat] {
			return fmt.Errorf("cat_threshold has %d words, cat_boundaries ends at %d", len(words), bounds[numCat])
		}
		t.catWords = make([]uint32, len(words))
		for i, w := range words {
			v, err := strconv.ParseUint(w, 10, 32)
			if err != nil {
				return fmt.Errorf("bad cat_threshold word %q", w)
			}
			t.catWords[i] = uint32(v)
		}
		for i := 0; i < numCat; i++ {
			if bounds[i] < 0 || bounds[i] > bounds[i+1] {
				return errors.New("cat_boundaries not increasing")
			}
		}
	}

	t.nodes = make([]node, n)
	for i := range t.nodes {
		if feat[i] < 0 || feat[i] >= nFeatures {
			return fmt.Errorf("node %d: split_feature %d out of range", i, feat[i])
		}
		// LightGBM numbers a new internal node after its parent, so a child
		// index is always greater. Checking that rules out cycles.
		for _, c := range []int{left[i], right[i]} {
			if c >= 0 && (c <= i || c >= n) || c < 0 && ^c >= numLeaves {
				return fmt.Errorf("node %d: child %d out of range", i, c)
			}
		}
		nd := node{
			feature:     int32(feat[i]),
			left:        int32(left[i]),
			right:       int32(right[i]),
			threshold:   thr[i],
			categorical: dt[i]&categoricalMask != 0,
			defaultLeft: dt[i]&defaultLeftMask != 0,
			missing:     uint8(dt[i]>>2) & 3,
		}
		if nd.missing > missingNaN {
			return fmt.Errorf("node %d: decision_type %d has unknown missing type", i, dt[i])
		}
		if nd.categorical {
			// For a categorical node, threshold holds the index of its bitset.
			ci := int(thr[i])
			if float64(ci) != thr[i] || ci < 0 || ci >= numCat {
				return fmt.Errorf("node %d: categorical threshold %v is not a bitset index", i, thr[i])
			}
			nd.catLo, nd.catHi = int32(bounds[ci]), int32(bounds[ci+1])
		}
		t.nodes[i] = nd
	}
	return nil
}

func fields(b map[string]string, key string, n int) ([]string, error) {
	v, ok := b[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	f := strings.Fields(v)
	if len(f) != n {
		return nil, fmt.Errorf("%s has %d values, want %d", key, len(f), n)
	}
	return f, nil
}

func floats(b map[string]string, key string, n int) ([]float64, error) {
	f, err := fields(b, key, n)
	if err != nil {
		return nil, err
	}
	out := make([]float64, n)
	for i, s := range f {
		// LightGBM writes doubles with 17 significant digits, so a correctly
		// rounded parse recovers the exact bits it holds in memory.
		if out[i], err = strconv.ParseFloat(s, 64); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return out, nil
}

func ints(b map[string]string, key string, n int) ([]int, error) {
	f, err := fields(b, key, n)
	if err != nil {
		return nil, err
	}
	out := make([]int, n)
	for i, s := range f {
		if out[i], err = strconv.Atoi(s); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return out, nil
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// FeatureNames returns the model's input names in input order. Do not modify.
func (m *Model) FeatureNames() []string { return m.features }

// NumFeatures is the length every input vector must have.
func (m *Model) NumFeatures() int { return len(m.features) }

// NumTrees is the number of trees summed per prediction.
func (m *Model) NumTrees() int { return len(m.trees) }

// Sigmoid is the objective's sigmoid parameter (1 unless trained otherwise).
func (m *Model) Sigmoid() float64 { return m.sigmoid }
