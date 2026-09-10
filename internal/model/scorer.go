package model

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Files in a model directory, all written by python/train.py.
const (
	ModelFile       = "model.txt"        // Booster.save_model(num_iteration=best)
	EncoderFile     = "encoder.json"     // schema.Encoder, copied from the Go export
	CalibrationFile = "calibration.json" // Calibrator
	MetadataFile    = "metadata.json"    // Metadata
)

// Metadata is the part of metadata.json the service reads. The file holds
// more (training parameters, data provenance) for people.
type Metadata struct {
	FeatureNames []string        `json:"feature_names"`
	FeatureStats map[string]Stat `json:"feature_stats"`
	Data         struct {
		Synthetic bool `json:"synthetic"`
	} `json:"data"`
}

// Scorer turns a catalog row into a risk score: encode, sum the trees,
// calibrate. It is safe for concurrent use.
type Scorer struct {
	enc     *schema.Encoder
	model   *Model
	cal     *Calibrator
	meta    Metadata
	fields  []schema.Field // catalog field of each model input
	stats   []*Stat        // per input, nil if unknown
	scratch sync.Pool      // *[]float64 of length NumFeatures
}

// LoadScorer loads a model directory and checks that the model, encoder and
// metadata agree on the input features, in order.
func LoadScorer(dir string, c *schema.Catalog) (*Scorer, error) {
	enc, err := schema.LoadEncoder(filepath.Join(dir, EncoderFile), c)
	if err != nil {
		return nil, err
	}
	m, err := Load(filepath.Join(dir, ModelFile))
	if err != nil {
		return nil, err
	}
	cal, err := LoadCalibrator(filepath.Join(dir, CalibrationFile))
	if err != nil {
		return nil, err
	}
	var meta Metadata
	b, err := os.ReadFile(filepath.Join(dir, MetadataFile))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("%s: %w", MetadataFile, err)
	}
	return NewScorer(c, enc, m, cal, meta)
}

// NewScorer assembles a Scorer from parts already loaded.
func NewScorer(c *schema.Catalog, enc *schema.Encoder, m *Model, cal *Calibrator, meta Metadata) (*Scorer, error) {
	if !slices.Equal(enc.Features, m.FeatureNames()) {
		return nil, fmt.Errorf("model: encoder features %v do not match model features %v", enc.Features, m.FeatureNames())
	}
	if meta.FeatureNames != nil && !slices.Equal(meta.FeatureNames, m.FeatureNames()) {
		return nil, fmt.Errorf("model: metadata features do not match model features")
	}
	s := &Scorer{enc: enc, model: m, cal: cal, meta: meta}
	n := m.NumFeatures()
	for i, name := range m.FeatureNames() {
		f, ok := c.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("model: feature %q is not in the catalog", name)
		}
		if (f.Kind == schema.String) != enc.IsCategorical(i) {
			return nil, fmt.Errorf("model: feature %q kind mismatch", name)
		}
		s.fields = append(s.fields, f)
		var st *Stat
		if v, ok := meta.FeatureStats[name]; ok {
			st = &v
		}
		s.stats = append(s.stats, st)
	}
	s.scratch.New = func() any { b := make([]float64, n); return &b }
	return s, nil
}

// Model returns the underlying tree model.
func (s *Scorer) Model() *Model { return s.model }

// Synthetic reports whether the model was trained on synthetic data.
func (s *Scorer) Synthetic() bool { return s.meta.Data.Synthetic }

// Score returns the 0-99 risk score, the calibrated probability behind it,
// and the model's raw log-odds. It does not allocate in steady state.
func (s *Scorer) Score(row schema.Row) (riskScore int, prob, raw float64) {
	bp := s.scratch.Get().(*[]float64)
	x := *bp
	s.enc.Encode(row, x)
	raw = s.model.PredictRaw(x)
	s.scratch.Put(bp)
	prob = s.cal.Apply(raw)
	return RiskScore(prob), prob, raw
}

// Contributions writes the model input vector for row to x and its Saabas
// contributions to out (both NumFeatures long, in FeatureNames order) and
// returns the bias. This is what the decision log records.
func (s *Scorer) Contributions(row schema.Row, x, out []float64) (bias float64) {
	s.enc.Encode(row, x)
	return s.model.Contributions(x, out)
}

// ScoreContributions is Score and Contributions from one walk of each tree
// (Model.PredictContributions): it writes row's model inputs to x and their
// Saabas contributions to out, and returns what Score returns plus the bias.
// raw has exactly the bits Score gives. It does not allocate.
func (s *Scorer) ScoreContributions(row schema.Row, x, out []float64) (riskScore int, prob, raw, bias float64) {
	s.enc.Encode(row, x)
	raw, bias = s.model.PredictContributions(x, out)
	prob = s.cal.Apply(raw)
	return RiskScore(prob), prob, raw, bias
}

// MaxReasons is how many reasons Explain returns at most.
const MaxReasons = 3

// Explain returns up to MaxReasons plain-English reasons for row's score:
// the inputs with the largest positive Saabas contributions, largest first.
// Inputs that pushed the score down are not reasons for it and are left out.
func (s *Scorer) Explain(row schema.Row) []Reason {
	n := s.model.NumFeatures()
	x := make([]float64, n)
	contrib := make([]float64, n)
	s.Contributions(row, x, contrib)
	return s.ExplainContributions(row, x, contrib, MaxReasons)
}

// NumFeatures is the number of model inputs, the length Contributions needs
// for its x and out buffers.
func (s *Scorer) NumFeatures() int { return s.model.NumFeatures() }

// FeatureNames returns the model inputs in order. Do not modify the result.
func (s *Scorer) FeatureNames() []string { return s.model.FeatureNames() }

// ExplainContributions is Explain for a caller that already has row's model
// inputs x and Saabas contributions (from ScoreContributions), such as the
// service, which logs the contributions and so computes them anyway. It
// returns at most limit reasons.
func (s *Scorer) ExplainContributions(row schema.Row, x, contrib []float64, limit int) []Reason {
	return s.AppendReasons(nil, row, x, contrib, limit)
}

// AppendReasons is ExplainContributions appending to dst, for a caller that
// reuses a buffer.
func (s *Scorer) AppendReasons(dst []Reason, row schema.Row, x, contrib []float64, limit int) []Reason {
	var buf [MaxReasons]int
	for _, i := range topPositive(contrib, limit, buf[:0]) {
		f := s.fields[i]
		var str string
		if f.Kind == schema.String {
			str = row.Str[f.Slot]
		}
		v := x[i]
		if f.Kind == schema.Number {
			v = row.Num[f.Slot]
		}
		dst = append(dst, Reason{
			Feature:      f.Name,
			Contribution: contrib[i],
			Text:         describe(f, v, str, !math.IsNaN(x[i]), s.stats[i]),
		})
	}
	return dst
}

// topPositive appends to top the indexes of the (at most) limit largest
// positive values in contrib, largest first, ties in index order. It keeps
// top sorted as it scans, which for a handful of reasons out of a few dozen
// inputs is cheaper than sorting them all.
func topPositive(contrib []float64, limit int, top []int) []int {
	if limit <= 0 {
		return top
	}
	for i, c := range contrib {
		if !(c > 0) {
			continue
		}
		// Insert after every kept value >= c: that is before the first
		// smaller one, so equal values stay in index order.
		k := len(top)
		for k > 0 && contrib[top[k-1]] < c {
			k--
		}
		if k == limit {
			continue
		}
		if len(top) < limit {
			top = append(top, 0)
		}
		copy(top[k+1:], top[k:])
		top[k] = i
	}
	return top
}
