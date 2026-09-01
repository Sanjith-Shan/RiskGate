package backtest

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// DefaultMaturity is how recent a payment can be and still count in a
// backtest. Fraud labels arrive as disputes, weeks after the payment, so
// the most recent payments look cleaner than they are and a naive backtest
// understates what a rule catches (experiment 7 measures by how much).
//
// IEEE-CIS carries no label arrival times, so the window cannot be measured
// from the data; it is chosen from what the data does say plus one stated
// assumption. The data spans about 182 days, and 60 days leaves four months
// to backtest over. Under the delay model DefaultDelay assumes for
// experiment 7 (lognormal, median 30 days), about 92% of disputes have
// arrived after 60 days. Card networks allow disputes for up to 120 days,
// so no shorter window is complete; the report always states how many
// payments were excluded, and Options.Maturity changes it.
const DefaultMaturity = 60 * 24 * time.Hour

// Options configures a backtest.
type Options struct {
	// From and To bound the payments considered, as TransactionDT seconds:
	// [From, To). From == 0 means from the first payment in the table and
	// To == 0 through the last.
	From, To int64
	// AsOf is when the backtest is run, in TransactionDT seconds: labels
	// are as of this moment and the maturity window ends here. 0 means To.
	AsOf int64
	// Maturity excludes payments made less than this long before AsOf.
	// 0 means DefaultMaturity; a negative value disables the window.
	Maturity time.Duration
	// UseLabelTime counts a fraud label only if Table.LabelTime says it had
	// arrived by AsOf; a fraudulent payment whose dispute is still to come
	// counts as legitimate, as it would in a live system. For experiment 7,
	// on SIMULATED arrival times.
	UseLabelTime bool
	// Epoch turns TransactionDT into dates. Zero means DefaultEpoch.
	Epoch time.Time
	// Samples is the number of example payments to include: 0 means 10,
	// negative means none.
	Samples int
}

// Counts is a number of payments and their total amount.
type Counts struct {
	Count   int     `json:"count"`
	Dollars float64 `json:"dollars"`
}

// Outcome splits a set of payments by label.
type Outcome struct {
	All       Counts `json:"all"`
	Fraud     Counts `json:"fraud"`
	Legit     Counts `json:"legit"`
	Unlabeled Counts `json:"unlabeled,omitempty"`
}

// Overlap is how a proposed rule's matches intersect one rule in force.
type Overlap struct {
	Index  int    `json:"index"` // in the current rule set
	ID     string `json:"id"`
	Rule   string `json:"rule"`
	Action string `json:"action"`
	// Both is the number of payments both rules match. DecidedBy is how
	// many of the proposed rule's matches this rule decides today.
	Both      int     `json:"both"`
	Share     float64 `json:"share"` // Both / proposed matches
	DecidedBy int     `json:"decided_by"`
}

// Sample is one example payment for the report.
type Sample struct {
	ID       int64          `json:"id"`
	Time     time.Time      `json:"time"`
	Amount   float64        `json:"amount"`
	Label    string         `json:"label"`   // fraud, legit or unknown, as the backtest counted it
	Current  string         `json:"current"` // decision under the rules in force
	Proposed string         `json:"proposed"`
	Fields   map[string]any `json:"fields"` // nil for missing
}

// Report is what a proposed rule would have changed. It is plain data and
// serializes to JSON for the HTTP API; NaN never appears (undefined ratios
// are null).
type Report struct {
	Rule   string `json:"rule"`
	RuleID string `json:"rule_id"`
	Action string `json:"action"`
	Shadow bool   `json:"shadow,omitempty"`

	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	AsOf   time.Time `json:"as_of"`
	Epoch  time.Time `json:"epoch"`
	FromDT int64     `json:"from_dt"`
	ToDT   int64     `json:"to_dt"`
	AsOfDT int64     `json:"as_of_dt"`

	MaturityDays float64 `json:"maturity_days"`
	// Immature is the payments in the window excluded by the maturity
	// window.
	Immature     Counts `json:"immature"`
	UseLabelTime bool   `json:"use_label_time"`

	// Period is every payment the backtest counted.
	Period Outcome `json:"period"`
	// Matched is every counted payment the rule's condition holds for.
	Matched Outcome `json:"matched"`
	// Changed is the matched payments whose decision the rule changes:
	// newly blocked, newly reviewed, or (for an allow rule) let through.
	Changed Outcome `json:"changed"`

	// Precision is the fraud share of the changed payments with a label.
	Precision *float64 `json:"precision"`
	// FraudDollarShare and FraudCountShare are the changed fraud as a share
	// of all fraud in the period: caught, for block and review rules; let
	// through, for allow rules.
	FraudDollarShare *float64 `json:"fraud_dollar_share"`
	FraudCountShare  *float64 `json:"fraud_count_share"`

	// For an allow rule, the changed payments split by what the rules in
	// force do to them today. Following Stripe's backtest, these are
	// overrides.
	OverridesBlock  *Outcome `json:"overrides_block,omitempty"`
	OverridesReview *Outcome `json:"overrides_review,omitempty"`
	// For a block rule, the changed payments that were going to review and
	// would be blocked instead.
	FromReview *Outcome `json:"from_review,omitempty"`

	Overlaps []Overlap `json:"overlaps"`
	// Unreachable is set when the rule matches payments but changes none of
	// them, because rules in force already decide all of them; CoveredBy
	// names those rules.
	Unreachable bool      `json:"unreachable"`
	CoveredBy   []Overlap `json:"covered_by,omitempty"`
	Warnings    []string  `json:"warnings"`

	SampleFields []string `json:"sample_fields"`
	Samples      []Sample `json:"samples"`

	SummaryText string `json:"summary"`
}

// Backtester holds a table and the decisions of the rule set in force over
// it, so many proposed rules can be tested against the same baseline
// without re-evaluating it. It is safe for concurrent use.
type Backtester struct {
	Table     *Table
	Current   *rules.RuleSet
	Decisions *Decisions
	workers   int
}

// NewBacktester evaluates the current rule set over t once. current may be
// nil, meaning no rules are in force. workers <= 0 means GOMAXPROCS.
func NewBacktester(t *Table, current *rules.RuleSet, workers int) (*Backtester, error) {
	if current == nil {
		current = rules.NewRuleSet(0, nil)
	}
	d, err := EvaluateRuleSet(current, t, workers)
	if err != nil {
		return nil, err
	}
	return &Backtester{Table: t, Current: current, Decisions: d, workers: workers}, nil
}

// Backtest is NewBacktester followed by Run, for a single report.
func Backtest(t *Table, current *rules.RuleSet, proposed *rules.CompiledRule, opt Options) (*Report, error) {
	b, err := NewBacktester(t, current, 0)
	if err != nil {
		return nil, err
	}
	return b.Run(proposed, opt)
}

// window is a resolved Options.
type window struct {
	opt               Options
	from, to, asOf    int64
	cutoff            int64 // payments at or after it are immature
	epoch             time.Time
	counted, immature *Bitmap
	fraud, legit      *Bitmap // labels as the backtest counts them
}

func (b *Backtester) window(opt Options) (*window, error) {
	t := b.Table
	w := &window{opt: opt, from: opt.From, to: opt.To, asOf: opt.AsOf, epoch: opt.Epoch}
	// Zero bounds mean the table's own ends, so that the summary's "over
	// the N days" is the span of the data, not of the clock.
	if lo, hi := t.TimeSpan(); t.N > 0 {
		if w.from == 0 {
			w.from = lo
		}
		if w.to == 0 {
			w.to = hi + 1
		}
	} else if w.to == 0 {
		w.to = w.from + 1
	}
	if w.to <= w.from {
		return nil, fmt.Errorf("backtest: empty time range [%d, %d)", w.from, w.to)
	}
	if w.asOf == 0 {
		w.asOf = w.to
	}
	if w.epoch.IsZero() {
		w.epoch = DefaultEpoch
	}
	maturity := opt.Maturity
	if maturity == 0 {
		maturity = DefaultMaturity
	}
	w.cutoff = math.MaxInt64
	if maturity > 0 {
		w.cutoff = w.asOf - int64(maturity/time.Second)
	}
	w.opt.Maturity = maturity
	if opt.UseLabelTime && t.LabelTime == nil {
		return nil, errors.New("backtest: UseLabelTime needs Table.LabelTime (see SimulateLabelTimes)")
	}
	w.counted, w.immature = NewBitmap(t.N), NewBitmap(t.N)
	w.fraud, w.legit = NewBitmap(t.N), NewBitmap(t.N)
	for i, dt := range t.DT {
		if dt < w.from || dt >= w.to {
			continue
		}
		if dt >= w.cutoff {
			w.immature.Set(i)
			continue
		}
		w.counted.Set(i)
		switch t.Fraud[i] {
		case Fraud:
			if opt.UseLabelTime && t.LabelTime[i] > w.asOf {
				w.legit.Set(i) // not disputed yet: it looks legitimate today
			} else {
				w.fraud.Set(i)
			}
		case Legit:
			w.legit.Set(i)
		}
	}
	return w, nil
}

func (b *Backtester) counts(m *Bitmap) Counts {
	c := Counts{Count: m.Count()}
	amt := b.Table.Amount
	m.ForEach(func(i int) { c.Dollars += amt[i] })
	return c
}

func (b *Backtester) outcome(m *Bitmap, w *window) Outcome {
	o := Outcome{All: b.counts(m)}
	o.Fraud = b.counts(m.Clone().And(w.fraud))
	o.Legit = b.counts(m.Clone().And(w.legit))
	o.Unlabeled = Counts{Count: o.All.Count - o.Fraud.Count - o.Legit.Count, Dollars: o.All.Dollars - o.Fraud.Dollars - o.Legit.Dollars}
	if o.Unlabeled.Count == 0 {
		o.Unlabeled.Dollars = 0 // not -0.0000001 of float residue
	}
	return o
}

func ratio(num, den float64) *float64 {
	if den == 0 {
		return nil
	}
	r := num / den
	return &r
}

// Run backtests a proposed rule against the rules in force. The proposed
// rule is treated as live even if it is written as a shadow rule, since the
// question is what it would do if enforced.
func (b *Backtester) Run(proposed *rules.CompiledRule, opt Options) (*Report, error) {
	t, d := b.Table, b.Decisions
	w, err := b.window(opt)
	if err != nil {
		return nil, err
	}
	prog, err := CompileVector(proposed.Rule.Cond, t)
	if err != nil {
		return nil, err
	}
	match := prog.EvalWorkers(b.workers).And(w.counted)

	// What changes follows from Radar's order: an allow rule changes a
	// payment the rules in force block or review; a block rule changes one
	// no allow rule protects and no block rule already blocks; a review
	// rule changes only a payment no rule decides today.
	changed := match.Clone()
	switch proposed.Action {
	case rules.Allow:
		changed.And(d.Blocked.Clone().Or(d.Reviewed))
	case rules.Block:
		changed.AndNot(d.Allowed).AndNot(d.Blocked)
	case rules.Review:
		changed.AndNot(d.Allowed).AndNot(d.Blocked).AndNot(d.Reviewed)
	default:
		return nil, fmt.Errorf("backtest: rule has no action")
	}

	r := &Report{
		Rule:         proposed.Text,
		RuleID:       proposed.ID,
		Action:       proposed.Action.String(),
		Shadow:       proposed.Shadow,
		Epoch:        w.epoch,
		FromDT:       w.from,
		ToDT:         w.to,
		AsOfDT:       w.asOf,
		From:         w.epoch.Add(time.Duration(w.from) * time.Second),
		To:           w.epoch.Add(time.Duration(w.to) * time.Second),
		AsOf:         w.epoch.Add(time.Duration(w.asOf) * time.Second),
		Immature:     b.counts(w.immature),
		UseLabelTime: opt.UseLabelTime,
		Period:       b.outcome(w.counted, w),
		Matched:      b.outcome(match, w),
		Changed:      b.outcome(changed, w),
		Warnings:     []string{},
		Overlaps:     []Overlap{},
		Samples:      []Sample{},
	}
	if w.opt.Maturity > 0 {
		r.MaturityDays = w.opt.Maturity.Hours() / 24
	}
	r.Precision = ratio(float64(r.Changed.Fraud.Count), float64(r.Changed.Fraud.Count+r.Changed.Legit.Count))
	r.FraudDollarShare = ratio(r.Changed.Fraud.Dollars, r.Period.Fraud.Dollars)
	r.FraudCountShare = ratio(float64(r.Changed.Fraud.Count), float64(r.Period.Fraud.Count))
	switch proposed.Action {
	case rules.Allow:
		ob := b.outcome(changed.Clone().And(d.Blocked), w)
		or := b.outcome(changed.Clone().And(d.Reviewed), w)
		r.OverridesBlock, r.OverridesReview = &ob, &or
	case rules.Block:
		fr := b.outcome(changed.Clone().And(d.Reviewed), w)
		r.FromReview = &fr
	}

	b.overlaps(r, match, changed)
	if r.Matched.All.Count == 0 {
		r.Warnings = append(r.Warnings, "This rule matches no payments in this period.")
	}
	if r.Unreachable {
		r.Warnings = append(r.Warnings, unreachableWarning(r))
	}
	if proposed.Shadow {
		r.Warnings = append(r.Warnings, "This is a shadow rule; the backtest shows what it would do if it were enforced.")
	}
	b.samples(r, proposed, changed, match, w)
	r.SummaryText = r.Summary()
	return r, nil
}

// overlaps fills Overlaps, Unreachable and CoveredBy.
func (b *Backtester) overlaps(r *Report, match, changed *Bitmap) {
	d := b.Decisions
	decidedBy := make(map[int32]int)
	defaultAllowed := 0
	match.ForEach(func(i int) {
		if d.Rule[i] >= 0 {
			decidedBy[d.Rule[i]]++
		} else {
			defaultAllowed++
		}
	})
	n := r.Matched.All.Count
	for i, cr := range b.Current.Rules {
		if cr.Shadow {
			continue
		}
		both := match.AndCount(d.Masks[i])
		if both == 0 {
			continue
		}
		r.Overlaps = append(r.Overlaps, Overlap{
			Index: i, ID: cr.ID, Rule: cr.Text, Action: cr.Action.String(),
			Both: both, Share: float64(both) / float64(n), DecidedBy: decidedBy[int32(i)],
		})
	}
	slices.SortStableFunc(r.Overlaps, func(a, b Overlap) int { return b.Both - a.Both })

	// Unreachable: the rule matches payments, but the rules in force decide
	// every one of them in a way this rule cannot change. The covering
	// rules are the ones deciding those payments today. (For an allow rule
	// the payments it matches are all allowed already, by a rule or by
	// default, and there is nothing to cover.)
	if n > 0 && !changed.Any() {
		r.Unreachable = true
		for _, o := range r.Overlaps {
			if o.DecidedBy > 0 {
				r.CoveredBy = append(r.CoveredBy, o)
			}
		}
		slices.SortStableFunc(r.CoveredBy, func(a, b Overlap) int { return b.DecidedBy - a.DecidedBy })
	}
}

func unreachableWarning(r *Report) string {
	if r.Action == "allow" {
		return fmt.Sprintf("This rule would not change any decision: the %s it matches are already allowed.",
			count(r.Matched.All.Count, "payment", "payments"))
	}
	var names []string
	for _, c := range r.CoveredBy {
		names = append(names, fmt.Sprintf("%q (%s)", c.Rule, count(c.DecidedBy, "payment", "payments")))
	}
	return fmt.Sprintf("This rule would not change any decision: every payment it matches is already decided by %s %s. It is unreachable as long as %s in force.",
		pluralWord(len(names), "the rule", "the rules"), strings.Join(names, ", "), pluralWord(len(names), "that rule is", "those rules are"))
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// sampleBase are the fields every sample shows, after the fields the rule
// itself reads.
var sampleBase = []string{"amount", "product_code", "card_network", "card_type", "purchaser_email_domain", "device_type", "risk_score"}

// samples picks up to opt.Samples payments the rule changes (or, if it
// changes none, that it matches), spread evenly over the period rather
// than the first few, so they show the rule's typical catch.
func (b *Backtester) samples(r *Report, proposed *rules.CompiledRule, changed, match *Bitmap, w *window) {
	t, d := b.Table, b.Decisions
	want := w.opt.Samples
	if want == 0 {
		want = 10
	}
	var fields []string
	seen := map[string]bool{}
	rules.Inspect(proposed.Rule.Cond, func(e rules.Expr) bool {
		if a, ok := e.(*rules.Attr); ok && !seen[a.Name] {
			seen[a.Name] = true
			fields = append(fields, a.Name)
		}
		return true
	})
	for _, f := range sampleBase {
		if _, ok := t.Catalog.Lookup(f); ok && !seen[f] {
			seen[f] = true
			fields = append(fields, f)
		}
	}
	r.SampleFields = fields
	if want < 0 {
		return
	}
	pool := changed
	if !pool.Any() {
		pool = match
	}
	rowsSet := pool.Rows()
	if len(rowsSet) == 0 {
		return
	}
	k := min(want, len(rowsSet))
	for j := range k {
		i := rowsSet[j*len(rowsSet)/k]
		s := Sample{
			ID:       t.ID[i],
			Time:     w.epoch.Add(time.Duration(t.DT[i]) * time.Second),
			Amount:   t.Amount[i],
			Label:    "unknown",
			Current:  d.Action[i].String(),
			Proposed: d.Action[i].String(),
			Fields:   make(map[string]any, len(fields)),
		}
		if w.fraud.Get(i) {
			s.Label = "fraud"
		} else if w.legit.Get(i) {
			s.Label = "legit"
		}
		if changed.Get(i) {
			s.Proposed = proposed.Action.String()
		}
		for _, name := range fields {
			f := t.Catalog.MustLookup(name)
			s.Fields[name] = fieldValue(t, f, i)
		}
		r.Samples = append(r.Samples, s)
	}
}

// fieldValue is a row's value of f as JSON can carry it: nil for missing,
// and infinities as strings, since JSON has no number for them.
func fieldValue(t *Table, f schema.Field, i int) any {
	if f.Kind == schema.String {
		if code := t.Str[f.Slot][i]; code != 0 {
			return t.Dict[f.Slot][code]
		}
		return nil
	}
	v := t.Num[f.Slot][i]
	switch {
	case math.IsNaN(v):
		return nil
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return v
}

// Summary describes the report in plain English, in the voice of the
// merchant who wrote the rule.
func (r *Report) Summary() string {
	var s []string
	say := func(format string, args ...any) { s = append(s, fmt.Sprintf(format, args...)) }
	period := fmt.Sprintf("Over the %s ending %s", days(r.ToDT-r.FromDT), date(r.Epoch, r.ToDT-1))
	ch := r.Changed
	one := ch.All.Count == 1
	disputed := "later disputed as fraud"
	if r.UseLabelTime {
		disputed = "disputed as fraud by " + date(r.Epoch, r.AsOfDT)
	}
	fraudShare := ""
	if r.Period.Fraud.Dollars > 0 && ch.Fraud.Count > 0 {
		fraudShare = fmt.Sprintf(", which is %s of all fraud dollars in that period", percent(ch.Fraud.Dollars, r.Period.Fraud.Dollars))
	}

	switch {
	case r.Unreachable:
		say("%s this rule matches %s, but the rules in force already decide %s, so it would change nothing.",
			period, count(r.Matched.All.Count, "payment", "payments"), pluralWord(r.Matched.All.Count, "it", "all of them"))
	case r.Action == "allow":
		say("%s this rule would have let through %s worth %s that the current rules block or send to review.",
			period, count(ch.All.Count, "payment", "payments"), dollars(ch.All.Dollars))
		switch {
		case ch.Fraud.Count == 0 && ch.Legit.Count > 0:
			say("%s %s.", pluralWord(ch.All.Count, "It was not", "None of them were"), disputed)
		case one && ch.Fraud.Count == 1:
			say("It was %s%s.", disputed, fraudShare)
		case ch.Fraud.Count > 0:
			say("%s of them (%s), worth %s, %s %s%s.", commas(int64(ch.Fraud.Count)),
				percent(float64(ch.Fraud.Count), float64(ch.Fraud.Count+ch.Legit.Count)),
				dollars(ch.Fraud.Dollars), pluralWord(ch.Fraud.Count, "was", "were"), disputed, fraudShare)
		}
		if ob := r.OverridesBlock; ob != nil && ob.All.Count > 0 {
			switch {
			case ob.All.Count == ch.All.Count:
				say("%s blocked today.", pluralWord(ch.All.Count, "It is", "All of them are"))
			default:
				say("%s of them %s blocked today and the rest go to review.", commas(int64(ob.All.Count)), pluralWord(ob.All.Count, "is", "are"))
			}
		}
	default: // block, review
		verb, dest := "blocked", ""
		if r.Action == "review" {
			verb, dest = "sent", " to review"
		}
		say("%s this rule would have %s %s worth %s%s.", period, verb, count(ch.All.Count, "payment", "payments"), dollars(ch.All.Dollars), dest)
		switch {
		case ch.Fraud.Count == 0 && ch.Legit.Count > 0:
			say("%s %s.", pluralWord(ch.All.Count, "It was not", "None of them were"), disputed)
		case one && ch.Fraud.Count == 1:
			say("It was %s%s.", disputed, fraudShare)
		case ch.Fraud.Count > 0:
			say("%s of them were %s%s.", percent(float64(ch.Fraud.Count), float64(ch.Fraud.Count+ch.Legit.Count)), disputed, fraudShare)
			if ch.Legit.Count > 0 {
				say("It would also have %s %s worth %s%s.", verb, count(ch.Legit.Count, "legitimate payment", "legitimate payments"), dollars(ch.Legit.Dollars), dest)
			}
		}
		if fr := r.FromReview; fr != nil && fr.All.Count > 0 {
			switch {
			case fr.All.Count == ch.All.Count:
				say("%s already going to review.", pluralWord(ch.All.Count, "It was", "All of them were"))
			default:
				say("%s of them %s already going to review.", commas(int64(fr.All.Count)), pluralWord(fr.All.Count, "was", "were"))
			}
		}
	}
	if extra := r.Matched.All.Count - ch.All.Count; extra > 0 && !r.Unreachable {
		already := map[string]string{"allow": "already allowed", "block": "already blocked or allowed by rules in force", "review": "already decided by rules in force"}[r.Action]
		say("It also matches %s %s.", count(extra, "payment", "payments"), pluralWord(extra, "that is "+already, "that are "+already))
	}
	if r.Immature.Count > 0 {
		say("%s from the last %s %s excluded because %s may not have arrived yet.",
			capitalize(count(r.Immature.Count, "payment", "payments")), days(int64(math.Round(r.MaturityDays*86400))),
			pluralWord(r.Immature.Count, "was", "were"), pluralWord(r.Immature.Count, "its dispute", "their disputes"))
	}
	if r.UseLabelTime {
		say("Dispute arrival times are SIMULATED.")
	}
	return strings.Join(s, " ")
}
