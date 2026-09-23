package backtest

import (
	"fmt"
	"math"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// SweepPoint is what `block if :risk_score: >= Threshold` would catch.
type SweepPoint struct {
	Threshold int    `json:"threshold"`
	Matched   Counts `json:"matched"`
	Fraud     Counts `json:"fraud"`
	Legit     Counts `json:"legit"`
	// Precision is Fraud / (Fraud + Legit) by count. The recalls are the
	// share of all fraud in the period, by dollars and by count.
	Precision         *float64 `json:"precision"`
	FraudDollarRecall *float64 `json:"fraud_dollar_recall"`
	FraudCountRecall  *float64 `json:"fraud_count_recall"`
}

// SweepResult is the threshold sweep for :risk_score:, 0 through 99.
type SweepResult struct {
	Period Outcome `json:"period"`
	// GivenCurrent is set when payments an allow rule in force protects
	// were left out (a block rule could never reach them); Protected
	// counts them.
	GivenCurrent bool         `json:"given_current"`
	Protected    Counts       `json:"protected"`
	Unscored     Counts       `json:"unscored"` // risk_score missing: no threshold matches
	Points       []SweepPoint `json:"points"`
}

// Sweep answers "which risk_score threshold should I pick": for every
// integer threshold k in 0..99, what `:risk_score: >= k` would match, how
// much of it is fraud, and what share of all fraud it catches, over the
// same window, maturity and labels as Run.
//
// It is one pass over the table, not a hundred: for an integer k,
// score >= k exactly when floor(score) >= k, so the payments go into 100
// buckets by floor(score) (anything at or above 99 into the last), and the
// answer for k is the sum of buckets k..99, a suffix sum. A missing score
// matches no threshold, as the rule language says.
//
// With givenCurrent, payments an allow rule in force protects are left out,
// since no block rule can touch them. Blocks already made by the current
// rules are counted as usual, so the sweep describes the threshold rule on
// its own terms, the way an analyst reads a score cut-off.
func (b *Backtester) Sweep(opt Options, givenCurrent bool) (*SweepResult, error) {
	t := b.Table
	f, ok := t.Catalog.Lookup(schema.RiskScoreField.Name)
	if !ok || f.Kind != schema.Number {
		return nil, fmt.Errorf("backtest: catalog has no number field %s", schema.RiskScoreField.Name)
	}
	w, err := b.window(opt)
	if err != nil {
		return nil, err
	}
	res := &SweepResult{GivenCurrent: givenCurrent, Period: w.period}
	pop := w.counted
	if givenCurrent {
		res.Protected = b.counts(pop.Clone().And(b.Decisions.Allowed))
		pop = pop.Clone().AndNot(b.Decisions.Allowed)
	}

	const buckets = 100
	var all, fraud, legit [buckets]Counts
	score := t.Num[f.Slot]
	pop.ForEach(func(i int) {
		s := score[i]
		if math.IsNaN(s) {
			res.Unscored.Count++
			res.Unscored.Dollars += t.Amount[i]
			return
		}
		if s < 0 {
			return // below every threshold
		}
		k := buckets - 1
		if s < buckets-1 {
			k = int(s) // floor, for s >= 0
		}
		add := func(c *Counts) { c.Count++; c.Dollars += t.Amount[i] }
		add(&all[k])
		if w.fraud.Get(i) {
			add(&fraud[k])
		} else if w.legit.Get(i) {
			add(&legit[k])
		}
	})

	res.Points = make([]SweepPoint, buckets)
	var cum SweepPoint
	for k := buckets - 1; k >= 0; k-- {
		for _, p := range [3]struct{ into, from *Counts }{{&cum.Matched, &all[k]}, {&cum.Fraud, &fraud[k]}, {&cum.Legit, &legit[k]}} {
			p.into.Count += p.from.Count
			p.into.Dollars += p.from.Dollars
		}
		p := cum
		p.Threshold = k
		p.Precision = ratio(float64(p.Fraud.Count), float64(p.Fraud.Count+p.Legit.Count))
		p.FraudDollarRecall = ratio(p.Fraud.Dollars, res.Period.Fraud.Dollars)
		p.FraudCountRecall = ratio(float64(p.Fraud.Count), float64(res.Period.Fraud.Count))
		res.Points[k] = p
	}
	return res, nil
}
