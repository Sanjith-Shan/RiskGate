package backtest

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func mustLoad(t *testing.T, src string) *rules.RuleSet {
	t.Helper()
	rs, err := rules.Load(src, testEnv(), 1)
	if err != nil {
		t.Fatalf("%s\n%v", src, err)
	}
	return rs
}

func mustRule(t *testing.T, src string) *rules.CompiledRule {
	t.Helper()
	return mustLoad(t, src).Rules[0]
}

// TestRunMatchesOracle checks the report's arithmetic against the most
// direct statement of what it means: evaluate the rule set with and without
// the proposed rule, row by row, and count the rows whose action differs.
func TestRunMatchesOracle(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 4000, Seed: 21, Edge: 0.02})
	rows := tbl.Rows()
	rng := rand.New(rand.NewPCG(8, 9))
	lo, hi := tbl.TimeSpan()
	iters := 80
	if testing.Short() || raceEnabled {
		iters = 15
	}
	for it := range iters {
		var lines []string
		for range rng.IntN(12) {
			lines = append(lines, rules.Generate(rng, env, rng.IntN(3)).String())
		}
		current := mustLoad(t, strings.Join(lines, "\n"))
		proposedText := strings.TrimPrefix(rules.Generate(rng, env, rng.IntN(3)).String(), "shadow ")
		proposed := mustRule(t, proposedText)
		with := mustLoad(t, strings.Join(append(lines, proposedText), "\n"))

		opt := Options{From: lo + rng.Int64N(hi-lo)/2, Samples: 5}
		opt.To = opt.From + 1 + rng.Int64N(hi-opt.From)
		opt.Maturity = time.Duration(rng.IntN(40)) * 24 * time.Hour
		if it%3 == 0 {
			opt.Maturity = -1
		}
		b, err := NewBacktester(tbl, current, 0)
		if err != nil {
			t.Fatal(err)
		}
		r, err := b.Run(proposed, opt)
		if err != nil {
			t.Fatal(err)
		}

		var want Outcome
		var period Counts
		maturity := opt.Maturity
		if maturity == 0 {
			maturity = DefaultMaturity
		}
		cutoff := int64(math.MaxInt64)
		if maturity > 0 {
			cutoff = opt.To - int64(maturity/time.Second)
		}
		for i, row := range rows {
			dt := tbl.DT[i]
			if dt < opt.From || dt >= opt.To || dt >= cutoff {
				continue
			}
			period.Count++
			if current.Evaluate(row).Action == with.Evaluate(row).Action {
				continue
			}
			add := func(c *Counts) { c.Count++; c.Dollars += tbl.Amount[i] }
			add(&want.All)
			switch tbl.Fraud[i] {
			case Fraud:
				add(&want.Fraud)
			case Legit:
				add(&want.Legit)
			}
		}
		if r.Period.All.Count != period.Count {
			t.Fatalf("%s: period %d, oracle %d", proposedText, r.Period.All.Count, period.Count)
		}
		if !sameCounts(r.Changed.All, want.All) || !sameCounts(r.Changed.Fraud, want.Fraud) || !sameCounts(r.Changed.Legit, want.Legit) {
			t.Fatalf("%s\ncurrent:\n%s\nchanged %+v, oracle %+v", proposedText, strings.Join(lines, "\n"), r.Changed, want)
		}
		if r.Unreachable != (r.Matched.All.Count > 0 && want.All.Count == 0) {
			t.Fatalf("%s: unreachable = %v with %d matched and %d changed", proposedText, r.Unreachable, r.Matched.All.Count, want.All.Count)
		}
		if len(r.Samples) > 5 {
			t.Fatalf("%d samples, asked for 5", len(r.Samples))
		}
		if _, err := json.Marshal(r); err != nil {
			t.Fatalf("report does not serialize: %v", err)
		}
	}
}

func sameCounts(a, b Counts) bool {
	return a.Count == b.Count && math.Abs(a.Dollars-b.Dollars) <= 1e-6*math.Max(1, math.Abs(b.Dollars))
}

// handTable builds a small table whose every number can be checked by eye.
// Days are counted from the epoch; payments on day 100 and later are the
// "recent" ones a maturity window excludes.
func handTable(t *testing.T) *Table {
	t.Helper()
	cat := schema.Default()
	b := NewBuilder(cat, 16)
	score := cat.MustLookup("risk_score").Slot
	email := cat.MustLookup("purchaser_email_domain").Slot
	amountF := cat.MustLookup("amount").Slot
	for i, p := range []struct {
		day    int
		score  float64
		email  string
		amount float64
		fraud  int8
	}{
		{10, 95, "anonymous.com", 400, Fraud},
		{11, 92, "gmail.com", 250, Fraud},
		{12, 91, "gmail.com", 100, Legit},
		{13, 70, "anonymous.com", 1000, Fraud},
		{14, 60, "anonymous.com", 50.5, Legit},
		{15, 30, "yahoo.com", 20, Legit},
		{16, math.NaN(), "anonymous.com", 75, Fraud},
		{17, 85, "", 300, Legit},
		{18, 88, "yahoo.com", 125, Unknown},
		{105, 97, "anonymous.com", 999, Legit}, // recent: excluded by a 60-day window ending day 120
	} {
		r := cat.NewRow()
		r.Num[score], r.Str[email], r.Num[amountF] = p.score, p.email, p.amount
		b.Append(r, Meta{ID: int64(1000 + i), DT: int64(p.day) * 86400, Amount: p.amount, Fraud: p.fraud})
	}
	return b.Table()
}

func TestRunHandExample(t *testing.T) {
	tbl := handTable(t)
	current := mustLoad(t, `allow if :purchaser_email_domain: = "gmail.com" and :risk_score: < 92
block if :risk_score: >= 90
review if :risk_score: >= 80`)
	b, err := NewBacktester(tbl, current, 1)
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{From: 5 * 86400, To: 125 * 86400}

	// A block rule on anonymous.com: it matches days 10, 13, 14, 16 (day
	// 105 is immature). Day 10 is already blocked (score 95), so it newly
	// blocks days 13, 14 and 16: two fraud ($1,000 + $75), one legit
	// ($50.50).
	r, err := b.Run(mustRule(t, `block if :purchaser_email_domain: = "anonymous.com"`), opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Matched.All.Count != 4 || r.Changed.All.Count != 3 {
		t.Fatalf("matched %d changed %d, want 4 and 3", r.Matched.All.Count, r.Changed.All.Count)
	}
	if r.Changed.Fraud != (Counts{2, 1075}) || r.Changed.Legit != (Counts{1, 50.5}) {
		t.Fatalf("fraud %+v legit %+v", r.Changed.Fraud, r.Changed.Legit)
	}
	if r.Immature != (Counts{1, 999}) {
		t.Fatalf("immature %+v", r.Immature)
	}
	// All fraud in the period: 400 + 250 + 1000 + 75 = 1725.
	if r.Period.Fraud != (Counts{4, 1725}) {
		t.Fatalf("period fraud %+v", r.Period.Fraud)
	}
	if got := *r.Precision; math.Abs(got-2.0/3) > 1e-12 {
		t.Fatalf("precision %v", got)
	}
	if got := *r.FraudDollarShare; math.Abs(got-1075.0/1725) > 1e-12 {
		t.Fatalf("fraud dollar share %v", got)
	}
	want := "Over the 120 days ending April 4, 2018 this rule would have blocked 3 payments worth $1,126. " +
		"67% of them were later disputed as fraud, which is 62% of all fraud dollars in that period. " +
		"It would also have blocked 1 legitimate payment worth $50.50. " +
		"It also matches 1 payment that is already blocked or allowed by rules in force. " +
		"1 payment from the last 60 days was excluded because its dispute may not have arrived yet."
	if r.Summary() != want || r.SummaryText != want {
		t.Errorf("summary:\n got %s\nwant %s", r.Summary(), want)
	}
	// Day 10 matches both the block rule (which decides it) and the review
	// rule (which never gets to).
	if len(r.Overlaps) != 2 || r.Overlaps[0].Index != 1 || r.Overlaps[0].Both != 1 || r.Overlaps[0].DecidedBy != 1 ||
		r.Overlaps[1].Index != 2 || r.Overlaps[1].DecidedBy != 0 || r.Overlaps[1].Share != 0.25 {
		t.Errorf("overlaps %+v", r.Overlaps)
	}
	if len(r.Samples) != 3 || r.Samples[0].ID != 1003 || r.Samples[0].Proposed != "block" || r.Samples[0].Current != "allow" {
		t.Errorf("samples %+v", r.Samples)
	}
	if r.SampleFields[0] != "purchaser_email_domain" {
		t.Errorf("sample fields should start with the rule's own: %v", r.SampleFields)
	}

	// An allow rule overrides: gmail.com payments that are blocked today.
	// Day 11 (score 92, fraud $250) is blocked; day 12 is allowed by the
	// allow rule already.
	r, err = b.Run(mustRule(t, `allow if :purchaser_email_domain: = "gmail.com"`), opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed.All != (Counts{1, 250}) || r.OverridesBlock.All.Count != 1 || r.OverridesBlock.Fraud.Count != 1 || r.OverridesReview.All.Count != 0 {
		t.Fatalf("allow: changed %+v, overrides %+v / %+v", r.Changed, r.OverridesBlock, r.OverridesReview)
	}
	if !strings.Contains(r.Summary(), "let through 1 payment worth $250 that the current rules block or send to review.") ||
		!strings.Contains(r.Summary(), "It was later disputed as fraud") && !strings.Contains(r.Summary(), "1 of them (100%)") {
		t.Errorf("allow summary: %s", r.Summary())
	}

	// A review rule covered by the block rule is unreachable.
	r, err = b.Run(mustRule(t, `review if :risk_score: >= 93`), opt)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Unreachable || len(r.CoveredBy) != 1 || r.CoveredBy[0].Rule != "block if :risk_score: >= 90" {
		t.Fatalf("unreachable %v, covered by %+v", r.Unreachable, r.CoveredBy)
	}
	if len(r.Warnings) == 0 || !strings.Contains(r.Warnings[0], `"block if :risk_score: >= 90"`) {
		t.Errorf("warnings %q", r.Warnings)
	}

	// A review rule over the unscored and the 80s: days 16 (unscored, no),
	// 17 (85, already reviewed) and 18 (88, already reviewed).
	r, err = b.Run(mustRule(t, `review if :risk_score: >= 85 and :risk_score: < 90`), opt)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Unreachable || r.Changed.All.Count != 0 {
		t.Fatalf("review 85-90: %+v", r.Changed)
	}

	// A rule that matches nothing warns.
	r, err = b.Run(mustRule(t, `block if :amount: > 100000`), opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Unreachable || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "matches no payments") {
		t.Fatalf("no-match warnings %q", r.Warnings)
	}
	if r.Precision != nil || !strings.Contains(r.Summary(), "blocked 0 payments worth $0.") {
		t.Errorf("no-match summary: %s", r.Summary())
	}
}

func TestRunOptions(t *testing.T) {
	tbl := handTable(t)
	b, err := NewBacktester(tbl, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	rule := mustRule(t, `shadow review if :risk_score: >= 0`)
	if _, err := b.Run(rule, Options{From: 10, To: 5}); err == nil {
		t.Error("accepted an empty range")
	}
	if _, err := b.Run(rule, Options{UseLabelTime: true}); err == nil {
		t.Error("accepted UseLabelTime without label times")
	}
	// To == 0 means the whole table; Maturity < 0 keeps everything.
	r, err := b.Run(rule, Options{Maturity: -1, Samples: -1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Period.All.Count != tbl.N || r.Immature.Count != 0 || len(r.Samples) != 0 {
		t.Errorf("period %d immature %d samples %d", r.Period.All.Count, r.Immature.Count, len(r.Samples))
	}
	if !r.Shadow || !strings.Contains(strings.Join(r.Warnings, " "), "shadow rule") {
		t.Errorf("shadow not reported: %v", r.Warnings)
	}
	// Unknown labels are neither fraud nor legitimate.
	if r.Matched.Unlabeled.Count != 1 {
		t.Errorf("unlabeled %+v", r.Matched.Unlabeled)
	}
	b2, err := Backtest(tbl, nil, rule, Options{Maturity: -1})
	if err != nil || b2.Changed.All.Count != r.Changed.All.Count {
		t.Errorf("Backtest convenience: %v", err)
	}
	// Label-time mode: a fraud whose dispute arrives after AsOf counts as
	// legitimate.
	tbl.LabelTime = make([]int64, tbl.N)
	for i := range tbl.LabelTime {
		tbl.LabelTime[i] = NoLabelTime
		if tbl.Fraud[i] == Fraud {
			tbl.LabelTime[i] = tbl.DT[i] + 30*86400
		}
	}
	r, err = b.Run(rule, Options{To: 40 * 86400, Maturity: -1, UseLabelTime: true})
	if err != nil {
		t.Fatal(err)
	}
	// Frauds on days 10, 11, 13 arrive by day 40 (days 40, 41, 43: only
	// day 10's arrives in time); the others look legitimate.
	if r.Matched.Fraud.Count != 1 || !strings.Contains(r.Summary(), "SIMULATED") {
		t.Errorf("label-time fraud %+v: %s", r.Matched.Fraud, r.Summary())
	}
}

func TestSweepMatchesScans(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 5000, Seed: 31, Edge: 0.02})
	// Non-integer scores exercise the floor: 84.5 is below threshold 85.
	score := tbl.Num[tbl.Catalog.MustLookup("risk_score").Slot]
	for i := range score {
		if i%7 == 0 && !math.IsNaN(score[i]) {
			score[i] += 0.5
		}
		if i%101 == 0 {
			score[i] = 150 // above the scale: matches every threshold
		}
	}
	current := mustLoad(t, `allow if :purchaser_email_domain: in @trusted_domains and :risk_score: < 50`)
	b, err := NewBacktester(tbl, current, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, given := range []bool{false, true} {
		opt := Options{Maturity: 30 * 24 * time.Hour}
		sw, err := b.Sweep(opt, given)
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 100; k += 7 {
			rule := mustRule(t, "block if :risk_score: >= "+itoa(k))
			r, err := b.Run(rule, opt)
			if err != nil {
				t.Fatal(err)
			}
			p := sw.Points[k]
			// The sweep counts every match, the report only changes; with
			// no block rules in force, they differ by the protected rows.
			m := r.Matched
			if given {
				m = r.Changed
			}
			if p.Threshold != k || !sameCounts(p.Matched, m.All) || !sameCounts(p.Fraud, m.Fraud) || !sameCounts(p.Legit, m.Legit) {
				t.Fatalf("given=%v threshold %d: sweep %+v, scan %+v", given, k, p, m)
			}
			if r.Precision != nil && math.Abs(*p.Precision-float64(m.Fraud.Count)/float64(m.Fraud.Count+m.Legit.Count)) > 1e-12 {
				t.Fatalf("threshold %d precision", k)
			}
		}
		if _, err := json.Marshal(sw); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(n int) string { return commas(int64(n)) }

func TestSweepNeedsRiskScore(t *testing.T) {
	cat := schema.New([]schema.Field{{Name: "amount", Kind: schema.Number}})
	b, err := NewBacktester(NewBuilder(cat, 0).Table(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Sweep(Options{To: 1}, false); err == nil {
		t.Fatal("swept a catalog with no risk_score")
	}
}
