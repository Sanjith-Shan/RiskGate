package backtest

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSimulateLabelTimes(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 60000, Seed: 41})
	m := DefaultDelay
	lt := SimulateLabelTimes(tbl, m)
	var delays []float64
	for i, v := range lt {
		if tbl.Fraud[i] != Fraud {
			if v != NoLabelTime {
				t.Fatalf("row %d is not fraud but has a label time", i)
			}
			continue
		}
		if v == NoLabelTime {
			continue // past the 120-day cap
		}
		d := float64(v-tbl.DT[i]) / 86400
		if d < 0 || d > m.MaxDays {
			t.Fatalf("delay %v days", d)
		}
		delays = append(delays, d)
	}
	if len(delays) < 1000 {
		t.Fatalf("only %d fraud rows", len(delays))
	}
	sort.Float64s(delays)
	if med := delays[len(delays)/2]; math.Abs(med-m.MedianDays) > 1.5 {
		t.Errorf("median delay %.1f days, model says %v", med, m.MedianDays)
	}
	// The empirical share within 60 days matches the model's CDF.
	within := sort.SearchFloat64s(delays, 60)
	if got, want := float64(within)/float64(len(delays)), m.ArrivedWithin(60); math.Abs(got-want) > 0.02 {
		t.Errorf("within 60 days: %.3f, model %.3f", got, want)
	}
	if a := m.ArrivedWithin(60); a < 0.9 || a > 0.93 {
		t.Errorf("DefaultMaturity's justification says about 92%% arrive in 60 days; the model says %.3f", a)
	}
	if m.ArrivedWithin(0) != 0 || m.ArrivedWithin(1000) != m.ArrivedWithin(m.MaxDays) {
		t.Error("ArrivedWithin ends")
	}
	// Deterministic, and keyed by ID rather than position.
	again := SimulateLabelTimes(tbl, m)
	for i := range lt {
		if lt[i] != again[i] {
			t.Fatal("not deterministic")
		}
	}
	m.Seed++
	if other := SimulateLabelTimes(tbl, m); equalInts(other, lt) {
		t.Error("seed has no effect")
	}
}

func equalInts(a, b []int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCompareLabelDelay(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 60000, Seed: 42})
	b, err := NewBacktester(tbl, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	rule := mustRule(t, `block if :risk_score: >= 70`)
	_, hi := tbl.TimeSpan()
	if _, err := b.CompareLabelDelay(rule, DefaultDelay, hi+1, 30*24*time.Hour, DefaultMaturity); err == nil {
		t.Fatal("ran without label times")
	}
	tbl.LabelTime = SimulateLabelTimes(tbl, DefaultDelay)
	res, err := b.CompareLabelDelay(rule, DefaultDelay, hi+1, 30*24*time.Hour, DefaultMaturity)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Simulated || !strings.HasPrefix(res.Summary(), "SIMULATED") {
		t.Error("not labelled simulated")
	}
	// The naive backtest sees at most the true fraud, and with a 30-day
	// median delay over the last 30 days, far less.
	if res.Naive.Caught.Dollars > res.NaiveTruth.Caught.Dollars || *res.NaiveUnderstatement < 0.4 {
		t.Errorf("naive understatement %.2f", *res.NaiveUnderstatement)
	}
	// Maturing for 60 days shrinks the gap a lot.
	if *res.MaturedUnderstatement > 0.2 || *res.MaturedUnderstatement >= *res.NaiveUnderstatement {
		t.Errorf("matured understatement %.2f vs naive %.2f", *res.MaturedUnderstatement, *res.NaiveUnderstatement)
	}
	// The two truths see the same rule over different windows, and the
	// windows are where they should be.
	if !res.Naive.To.Equal(res.NaiveTruth.To) || !res.Matured.To.Equal(res.Naive.To.Add(-DefaultMaturity)) {
		t.Errorf("windows: naive to %v, matured to %v", res.Naive.To, res.Matured.To)
	}
	if res.Naive.Changed != res.NaiveTruth.Changed {
		t.Error("labels changed which payments the rule blocks")
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CompareLabelDelay(rule, DefaultDelay, hi, 0, DefaultMaturity); err == nil {
		t.Error("accepted a zero window")
	}
}
