package backtest

import (
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// The row-at-a-time path: the online closure evaluator applied to history,
// one schema.Row per payment. It is the reference the vectorized evaluator
// is tested against, and the baseline experiment 6 measures it against.

// MatchRows evaluates a compiled rule on pre-materialized rows (a row
// store). This is the row-at-a-time path at its best: the rows already
// exist and only the closures run.
func MatchRows(cr *rules.CompiledRule, rows []schema.Row) *Bitmap {
	b := NewBitmap(len(rows))
	for i, r := range rows {
		if cr.Match(r) {
			b.Set(i)
		}
	}
	return b
}

// MatchTable evaluates a compiled rule over the table one row at a time,
// decoding each row from the columns into a reused schema.Row first. That is
// what a row-oriented backtester reading this cache would do.
func MatchTable(cr *rules.CompiledRule, t *Table) *Bitmap {
	b := NewBitmap(t.N)
	row := t.Catalog.NewRow()
	for i := range t.N {
		t.Row(i, &row)
		if cr.Match(row) {
			b.Set(i)
		}
	}
	return b
}

// EvaluateRows is RuleSet.Evaluate on every row: the reference for
// EvaluateRuleSet.
func EvaluateRows(rs *rules.RuleSet, rows []schema.Row) []rules.Decision {
	out := make([]rules.Decision, len(rows))
	for i, r := range rows {
		out[i] = rs.Evaluate(r)
	}
	return out
}
