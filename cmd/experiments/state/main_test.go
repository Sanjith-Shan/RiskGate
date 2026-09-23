package main

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/data/synth"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestEvaluate checks the metrics against values worked out by hand with
// scikit-learn's definitions.
func TestEvaluate(t *testing.T) {
	m := evaluate([]bool{true, false, true, false}, []float64{0.9, 0.8, 0.7, 0.1})
	if !near(m.ROCAUC, 0.75) || !near(m.PRAUC, 0.5+0.5*2.0/3) || !near(m.RecallAt1PctFPR, 0.5) {
		t.Errorf("got %+v", m)
	}
	// Tied scores are one threshold: no partial credit for order.
	m = evaluate([]bool{true, false}, []float64{0.5, 0.5})
	if !near(m.ROCAUC, 0.5) || !near(m.PRAUC, 0.5) || m.RecallAt1PctFPR != 0 {
		t.Errorf("ties: got %+v", m)
	}
	// A perfect ranking.
	m = evaluate([]bool{false, true, true, false}, []float64{1, 3, 4, 2})
	if !near(m.ROCAUC, 1) || !near(m.PRAUC, 1) || !near(m.RecallAt1PctFPR, 1) {
		t.Errorf("perfect: got %+v", m)
	}
	if m := evaluate([]bool{true, true}, []float64{1, 2}); m != (ModelEval{}) {
		t.Errorf("one class: got %+v", m)
	}
}

func TestFamily(t *testing.T) {
	for name, want := range map[string]string{
		"card_txn_count_1h":             "count",
		"uid_amount_sum_7d":             "sum",
		"distinct_cards_per_email_24h":  "distinct",
		"device_seconds_since_last":     "recency",
		"email_amount_ratio_7d":         "derived (mean, ratio)",
		"card_mean_amount_7d":           "derived (mean, ratio)",
		"distinct_cards_per_device_24h": "distinct",
	} {
		if got := family(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

// TestShootoutSynthetic runs every part except the model metrics on a small
// generated dataset.
func TestShootoutSynthetic(t *testing.T) {
	txns, _ := synth.Generate(synth.Config{Rows: 4000, Days: 60, Seed: 3})
	ds := &data.Dataset{Txns: txns, Calendar: data.CalendarFor(txns), Synthetic: true}
	var log bytes.Buffer
	res, err := shootout(ds, kinds, nil, 1, func(s string) { log.WriteString(s + "\n") })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.States) != 3 || res.Rows != 4000 || res.KeyedOps < 4000 {
		t.Fatalf("result %+v", res)
	}
	for _, s := range res.States {
		if !s.Memory.EvictionIdentical {
			t.Errorf("%s: eviction changed a feature", s.Name)
		}
		if s.Memory.EndKeys <= 0 || s.Memory.EndBytes <= 0 || s.Timing.AddMedianNs <= 0 || s.Timing.ReadAddMedianNs <= 0 || len(s.Timing.ReadRuns) != 1 {
			t.Errorf("%s: %+v %+v", s.Name, s.Memory, s.Timing)
		}
		if (s.Name == "exact") != (s.Errors == nil) {
			t.Errorf("%s: error families %v", s.Name, s.Errors)
		}
		for _, f := range s.Errors {
			if f.ExactShare < 0 || f.ExactShare > 1 {
				t.Errorf("%s %s: exact share %v", s.Name, f.Family, f.ExactShare)
			}
		}
	}
	// The exact state is the reference, so the bucketed state never
	// undercounts it (DESIGN.md) and the result serializes.
	for _, f := range res.States[1].Errors {
		if f.Family == "count" && f.UnderShare != 0 {
			t.Errorf("bucketed counts under exact on %v of rows", f.UnderShare)
		}
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
}
