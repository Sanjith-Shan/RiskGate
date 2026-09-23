package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

// flagRows writes the rules-alone baseline of experiment 1: for every row of
// the table, TransactionID and whether the rule set flags it (1 when the
// final decision, in Radar's order, is block or review). An allow rule that
// matches wins, so an allowed payment is 0 whatever block or review rules
// say. python/train.py and python/evaluate.py read the file through
// --rules-predictions.
func flagRows(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("flag", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 100_000)
	rulesPath := fs.String("rules", "", "rule set file")
	path := fs.String("out", "", "CSV to write (TransactionID,flagged)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *rulesPath == "" || *path == "" {
		return errors.New("flag: -rules and -out are required")
	}
	rs, err := loadRulesFile(*rulesPath)
	if err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	d, err := backtest.EvaluateRuleSet(rs, t, 0)
	if err != nil {
		return err
	}
	f, err := os.Create(*path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "TransactionID,flagged")
	flagged := 0
	for i := range t.N {
		v := byte('0')
		if d.Action[i] == rules.Block || d.Action[i] == rules.Review {
			v = '1'
			flagged++
		}
		w.WriteString(strconv.FormatInt(t.ID[i], 10))
		w.WriteByte(',')
		w.WriteByte(v)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: %s rows, %s flagged (block or review) by %d rules\n", *path, commas(t.N), commas(flagged), len(rs.Rules))
	return nil
}

func loadRulesFile(path string) (*rules.RuleSet, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadRules(string(src))
}

// splitRows returns the rows of t in one split of the time split
// (data.Calendar anchored at the table's first day), or every row for "".
func splitRows(t *backtest.Table, split string) (*backtest.Bitmap, error) {
	all := backtest.NewBitmap(t.N).Fill()
	if split == "" || split == "all" {
		return all, nil
	}
	lo, _ := t.TimeSpan()
	cal := data.Calendar{Origin: lo / data.SecondsPerDay * data.SecondsPerDay} // DT is never negative
	want := map[string]data.Split{"train": data.Train, "valid": data.Valid, "test": data.Test}
	s, ok := want[split]
	if !ok {
		return nil, fmt.Errorf("unknown -split %q (train, valid, test or all)", split)
	}
	b := backtest.NewBitmap(t.N)
	for i, dt := range t.DT {
		if cal.Split(dt) == s {
			b.Set(i)
		}
	}
	return b, nil
}

// RuleStat is one rule's standalone effect in rulestats.
type RuleStat struct {
	Rule          string  `json:"rule"`
	Action        string  `json:"action"`
	Matched       int     `json:"matched"`
	Fraud         int     `json:"fraud"`
	Precision     float64 `json:"precision"`
	FraudRecall   float64 `json:"fraud_recall"`
	LegitFPR      float64 `json:"legit_fpr"`
	Decided       int     `json:"decided"`        // rows this rule decides in the set
	DecidedFraud  int     `json:"decided_fraud"`  // of those, fraud
	FraudDollars  float64 `json:"fraud_dollars"`  // standalone matched fraud dollars
	DecidedDollar float64 `json:"decided_dollar"` // fraud dollars of the rows it decides
}

// SetStat is the whole rule set's effect in rulestats: flagged means the
// final decision is block or review.
type SetStat struct {
	Split             string     `json:"split"`
	Rows              int        `json:"rows"`
	Fraud             int        `json:"fraud"`
	Flagged           int        `json:"flagged"`
	FlaggedFraud      int        `json:"flagged_fraud"`
	Precision         float64    `json:"precision"`
	Recall            float64    `json:"recall"`
	FPR               float64    `json:"fpr"`
	FraudDollarRecall float64    `json:"fraud_dollar_recall"`
	LegitDollarShare  float64    `json:"legit_dollar_share"` // legit dollars flagged / all legit dollars
	Allowed           int        `json:"allowed_by_rule"`
	Rules             []RuleStat `json:"rules"`
}

// ruleStats evaluates rs on the rows in scope and reports each rule alone
// and the set in Radar's order. Only aggregates are reported.
func ruleStats(t *backtest.Table, rs *rules.RuleSet, scope *backtest.Bitmap) (*SetStat, error) {
	d, err := backtest.EvaluateRuleSet(rs, t, 0)
	if err != nil {
		return nil, err
	}
	fraud := backtest.NewBitmap(t.N)
	var fraudUSD, legitUSD float64
	scope.ForEach(func(i int) {
		if t.Fraud[i] == backtest.Fraud {
			fraud.Set(i)
			fraudUSD += t.Amount[i]
		} else {
			legitUSD += t.Amount[i]
		}
	})
	nFraud, nRows := fraud.Count(), scope.Count()
	nLegit := nRows - nFraud
	div := func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return a / b
	}
	s := &SetStat{Rows: nRows, Fraud: nFraud}
	for i, r := range rs.Rules {
		m := d.Masks[i].Clone().And(scope)
		st := RuleStat{Rule: r.Text, Action: r.Action.String(), Matched: m.Count(), Fraud: m.AndCount(fraud)}
		if r.Shadow {
			st.Action = "shadow " + st.Action
		}
		st.Precision = div(float64(st.Fraud), float64(st.Matched))
		st.FraudRecall = div(float64(st.Fraud), float64(nFraud))
		st.LegitFPR = div(float64(st.Matched-st.Fraud), float64(nLegit))
		m.ForEach(func(row int) {
			isFraud := t.Fraud[row] == backtest.Fraud
			if isFraud {
				st.FraudDollars += t.Amount[row]
			}
			if int(d.Rule[row]) == i {
				st.Decided++
				if isFraud {
					st.DecidedFraud++
					st.DecidedDollar += t.Amount[row]
				}
			}
		})
		s.Rules = append(s.Rules, st)
	}
	var flaggedFraudUSD, flaggedLegitUSD float64
	scope.ForEach(func(i int) {
		a := d.Action[i]
		if a == rules.Allow && d.Rule[i] >= 0 {
			s.Allowed++
		}
		if a != rules.Block && a != rules.Review {
			return
		}
		s.Flagged++
		if fraud.Get(i) {
			s.FlaggedFraud++
			flaggedFraudUSD += t.Amount[i]
		} else {
			flaggedLegitUSD += t.Amount[i]
		}
	})
	s.Precision = div(float64(s.FlaggedFraud), float64(s.Flagged))
	s.Recall = div(float64(s.FlaggedFraud), float64(nFraud))
	s.FPR = div(float64(s.Flagged-s.FlaggedFraud), float64(nLegit))
	s.FraudDollarRecall = div(flaggedFraudUSD, fraudUSD)
	s.LegitDollarShare = div(flaggedLegitUSD, legitUSD)
	return s, nil
}

// rulestats prints each rule's standalone precision and recall and the rule
// set's combined effect over one split, for tuning a rule set on the
// validation month.
func rulestats(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("rulestats", flag.ContinueOnError)
	var tf tableFlags
	tf.register(fs, 100_000)
	rulesPath := fs.String("rules", "", "rule set file")
	split := fs.String("split", "valid", "rows to score: train, valid, test or all")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *rulesPath == "" {
		return errors.New("rulestats: -rules is required")
	}
	rs, err := loadRulesFile(*rulesPath)
	if err != nil {
		return err
	}
	t, err := tf.load(out)
	if err != nil {
		return err
	}
	scope, err := splitRows(t, *split)
	if err != nil {
		return err
	}
	s, err := ruleStats(t, rs, scope)
	if err != nil {
		return err
	}
	s.Split = *split
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", " ")
		return enc.Encode(s)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "action\tmatched\tfraud\tprecision\trecall\tFPR\tdecides\tdecided fraud\trule")
	for _, r := range s.Rules {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f%%\t%.2f%%\t%.3f%%\t%s\t%s\t%s\n", r.Action, commas(r.Matched), commas(r.Fraud),
			100*r.Precision, 100*r.FraudRecall, 100*r.LegitFPR, commas(r.Decided), commas(r.DecidedFraud), r.Rule)
	}
	tw.Flush()
	fmt.Fprintf(out, "\nsplit %s: %s rows, %s fraud. Flagged (block or review) %s: precision %.1f%%, recall %.2f%%, FPR %.3f%%, fraud-dollar recall %.2f%%, legit dollars flagged %.3f%%, allowed by a rule %s\n",
		*split, commas(s.Rows), commas(s.Fraud), commas(s.Flagged), 100*s.Precision, 100*s.Recall, 100*s.FPR,
		100*s.FraudDollarRecall, 100*s.LegitDollarShare, commas(s.Allowed))
	return nil
}
