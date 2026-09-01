package backtest

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

// Experiment 7: label delay. A fraud label is a dispute, and a dispute
// arrives weeks after the payment. IEEE-CIS labels are final and carry no
// arrival time, so the arrival time here is SIMULATED from a stated
// distribution, and every number computed from it is labelled simulated.

// DelayModel is the assumed distribution of the time from a fraudulent
// payment to its dispute: lognormal, which is right-skewed the way dispute
// timing is (most within a statement cycle or two, a long tail out to the
// networks' 120-day limit).
type DelayModel struct {
	MedianDays float64 `json:"median_days"`
	Sigma      float64 `json:"sigma"` // of the underlying normal
	// MaxDays caps delays at the card networks' dispute window; a draw
	// beyond it is a dispute that never arrives. 0 means no cap.
	MaxDays float64 `json:"max_days"`
	Seed    uint64  `json:"seed"`
}

// DefaultDelay is the ASSUMPTION used for experiment 7: median 30 days
// (cardholders mostly find unauthorized charges on their next statement),
// sigma 0.5 (so about 92% of disputes arrive within 60 days and 99.7%
// within the 120-day network limit), capped at 120 days. It is not
// measured from any data.
var DefaultDelay = DelayModel{MedianDays: 30, Sigma: 0.5, MaxDays: 120, Seed: 1}

// ArrivedWithin returns the modelled share of disputes that arrive within
// the given number of days.
func (m DelayModel) ArrivedWithin(days float64) float64 {
	if days <= 0 {
		return 0
	}
	if m.MaxDays > 0 && days > m.MaxDays {
		days = m.MaxDays
	}
	z := (math.Log(days) - math.Log(m.MedianDays)) / m.Sigma
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// SimulateLabelTimes returns a SIMULATED LabelTime for every row of t: for
// a fraudulent payment, its DT plus a delay drawn from m; for every other
// payment, NoLabelTime. The draw for a payment depends only on m.Seed and
// the payment's ID, so it does not change when the table is filtered or
// reordered.
func SimulateLabelTimes(t *Table, m DelayModel) []int64 {
	out := make([]int64, t.N)
	for i := range out {
		out[i] = NoLabelTime
		if t.Fraud[i] != Fraud {
			continue
		}
		days := m.MedianDays * math.Exp(m.Sigma*normal(m.Seed, uint64(t.ID[i])))
		if m.MaxDays > 0 && days > m.MaxDays {
			continue // past the dispute window: never arrives
		}
		out[i] = t.DT[i] + int64(math.Round(days*86400))
	}
	return out
}

// normal is a standard normal draw determined by (seed, id): two uniforms
// from splitmix64, then Box-Muller.
func normal(seed, id uint64) float64 {
	x := seed ^ id*0x9e3779b97f4a7c15
	u1 := (float64(splitmix(&x)>>11) + 0.5) / (1 << 53) // in (0, 1): log is finite
	u2 := float64(splitmix(&x)>>11) / (1 << 53)
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

func splitmix(x *uint64) uint64 {
	*x += 0x9e3779b97f4a7c15
	z := *x
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

// DelayView is one backtest of the rule in the label-delay comparison.
type DelayView struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Labels    string    `json:"labels"` // "as of <date>" or "all (final)"
	Changed   Counts    `json:"changed"`
	Caught    Counts    `json:"caught"` // changed payments counted as fraud
	Precision *float64  `json:"precision"`
	// FraudDollarShare is caught fraud dollars over all fraud dollars the
	// same view can see in its window.
	FraudDollarShare *float64 `json:"fraud_dollar_share"`
}

// LabelDelayResult compares a naive backtest over the most recent window
// with the truth, and a matured backtest (the same length of window, ending
// a maturity window earlier) with its truth. Every number is SIMULATED.
type LabelDelayResult struct {
	Simulated    bool       `json:"simulated"`
	Model        DelayModel `json:"model"`
	Rule         string     `json:"rule"`
	WindowDays   float64    `json:"window_days"`
	MaturityDays float64    `json:"maturity_days"`

	Naive        DelayView `json:"naive"`         // last window, labels as of AsOf
	NaiveTruth   DelayView `json:"naive_truth"`   // same window, every label
	Matured      DelayView `json:"matured"`       // window ending AsOf - maturity, labels as of AsOf
	MaturedTruth DelayView `json:"matured_truth"` // same window, every label

	// Understatement is 1 - reported / true caught fraud dollars.
	NaiveUnderstatement   *float64 `json:"naive_understatement"`
	MaturedUnderstatement *float64 `json:"matured_understatement"`
	SummaryText           string   `json:"summary"`
}

// CompareLabelDelay runs experiment 7 for one rule. The table must carry
// LabelTime (SimulateLabelTimes). asOf is "today" in TransactionDT seconds.
func (b *Backtester) CompareLabelDelay(proposed *rules.CompiledRule, model DelayModel, asOf int64, window, maturity time.Duration) (*LabelDelayResult, error) {
	if b.Table.LabelTime == nil {
		return nil, errors.New("backtest: the table has no LabelTime; call SimulateLabelTimes first")
	}
	if window <= 0 || maturity <= 0 {
		return nil, errors.New("backtest: window and maturity must be positive")
	}
	w, m := int64(window/time.Second), int64(maturity/time.Second)
	view := func(from, to int64, labelTime bool) (DelayView, error) {
		r, err := b.Run(proposed, Options{From: from, To: to, AsOf: asOf, Maturity: -1, UseLabelTime: labelTime, Samples: -1})
		if err != nil {
			return DelayView{}, err
		}
		v := DelayView{From: r.From, To: r.To, Changed: r.Changed.All, Caught: r.Changed.Fraud,
			Precision: r.Precision, FraudDollarShare: r.FraudDollarShare, Labels: "all (final)"}
		if labelTime {
			v.Labels = "as of " + date(r.Epoch, asOf)
		}
		return v, nil
	}
	res := &LabelDelayResult{Simulated: true, Model: model, Rule: proposed.Text,
		WindowDays: window.Hours() / 24, MaturityDays: maturity.Hours() / 24}
	var err error
	for _, v := range []struct {
		into      *DelayView
		from, to  int64
		labelTime bool
	}{
		{&res.Naive, asOf - w, asOf, true},
		{&res.NaiveTruth, asOf - w, asOf, false},
		{&res.Matured, asOf - m - w, asOf - m, true},
		{&res.MaturedTruth, asOf - m - w, asOf - m, false},
	} {
		if *v.into, err = view(v.from, v.to, v.labelTime); err != nil {
			return nil, err
		}
	}
	under := func(got, truth DelayView) *float64 {
		if truth.Caught.Dollars == 0 {
			return nil
		}
		u := 1 - got.Caught.Dollars/truth.Caught.Dollars
		return &u
	}
	res.NaiveUnderstatement = under(res.Naive, res.NaiveTruth)
	res.MaturedUnderstatement = under(res.Matured, res.MaturedTruth)
	res.SummaryText = res.Summary()
	return res, nil
}

// Summary states the comparison in plain English, marked SIMULATED.
func (r *LabelDelayResult) Summary() string {
	end := func(v DelayView) string { return v.To.Add(-time.Second).Format("January 2, 2006") }
	s := fmt.Sprintf("SIMULATED (dispute delays drawn from a lognormal with median %g days, an assumption). ", r.Model.MedianDays)
	s += fmt.Sprintf("A naive backtest of the %s ending %s credits this rule with %s of fraud caught; with every dispute in, it catches %s",
		days(int64(r.WindowDays*86400)), end(r.Naive), dollars(r.Naive.Caught.Dollars), dollars(r.NaiveTruth.Caught.Dollars))
	if r.NaiveUnderstatement != nil {
		s += fmt.Sprintf(", so the naive backtest understates it by %s", percent(*r.NaiveUnderstatement, 1))
	}
	s += fmt.Sprintf(". Leaving out the last %s and backtesting the %s ending %s instead credits %s against a true %s",
		days(int64(r.MaturityDays*86400)), days(int64(r.WindowDays*86400)), end(r.Matured), dollars(r.Matured.Caught.Dollars), dollars(r.MaturedTruth.Caught.Dollars))
	if r.MaturedUnderstatement != nil {
		s += fmt.Sprintf(", an understatement of %s", percent(*r.MaturedUnderstatement, 1))
	}
	return s + "."
}
