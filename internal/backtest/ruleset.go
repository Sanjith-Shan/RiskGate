package backtest

import (
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

// Decisions is a rule set's verdict on every row of a table, computed with
// bitmaps and equal, row for row, to rules.RuleSet.Evaluate.
type Decisions struct {
	Set *rules.RuleSet
	N   int
	// Action is the decision per row: Allow (by a rule or by default),
	// Block or Review.
	Action []rules.Action
	// Rule is the index into Set.Rules of the rule that decided each row, or
	// -1 when no live rule matched and the payment is allowed by default.
	Rule []int32
	// Masks holds, per rule in Set.Rules (shadow rules included), the rows
	// where its condition holds, whatever the final decision.
	Masks []*Bitmap
	// Allowed holds the rows an allow rule matched. Blocked and Reviewed
	// hold the rows with those final decisions. Rows in none of the three
	// are allowed by default.
	Allowed, Blocked, Reviewed *Bitmap
}

// CompileRuleSet compiles every rule of rs, shadow rules included, against t.
func CompileRuleSet(rs *rules.RuleSet, t *Table) ([]*Program, error) {
	progs := make([]*Program, len(rs.Rules))
	for i, r := range rs.Rules {
		p, err := CompileVector(r.Rule.Cond, t)
		if err != nil {
			return nil, err
		}
		progs[i] = p
	}
	return progs, nil
}

// EvaluateRuleSet decides every row of t with rs, in Radar's order, with
// vectorized evaluation (workers <= 0 means GOMAXPROCS):
//
//	allowed  = OR(allow rules)
//	blocked  = OR(block rules)  AND NOT allowed
//	reviewed = OR(review rules) AND NOT allowed AND NOT blocked
//
// and, within an action, each row is attributed to the first rule in source
// order that matched it, which is what RuleSet.Evaluate reports.
func EvaluateRuleSet(rs *rules.RuleSet, t *Table, workers int) (*Decisions, error) {
	progs, err := CompileRuleSet(rs, t)
	if err != nil {
		return nil, err
	}
	masks := EvalMany(t, progs, 0, t.N, workers)
	return decide(rs, t.N, masks), nil
}

// decide turns per-rule masks into decisions.
func decide(rs *rules.RuleSet, n int, masks []*Bitmap) *Decisions {
	d := &Decisions{
		Set:      rs,
		N:        n,
		Action:   make([]rules.Action, n),
		Rule:     make([]int32, n),
		Masks:    masks,
		Allowed:  NewBitmap(n),
		Blocked:  NewBitmap(n),
		Reviewed: NewBitmap(n),
	}
	for i := range d.Action {
		d.Action[i] = rules.Allow
		d.Rule[i] = -1
	}
	// decided accumulates every row a live rule has claimed so far; since
	// allow is attributed before block and block before review, "not yet
	// decided" is exactly Radar's eligibility for the next action, and
	// within an action it makes the first rule in source order win.
	decided := NewBitmap(n)
	newly := NewBitmap(n)
	for _, step := range [3]struct {
		action rules.Action
		into   *Bitmap
	}{{rules.Allow, d.Allowed}, {rules.Block, d.Blocked}, {rules.Review, d.Reviewed}} {
		for i, r := range rs.Rules {
			if r.Shadow || r.Action != step.action {
				continue
			}
			copy(newly.W, masks[i].W)
			newly.AndNot(decided)
			newly.ForEach(func(row int) {
				d.Action[row] = step.action
				d.Rule[row] = int32(i)
			})
			decided.Or(newly)
			step.into.Or(newly)
		}
	}
	return d
}

// DecidingRule returns the rule that decided row i, or nil for the default.
func (d *Decisions) DecidingRule(i int) *rules.CompiledRule {
	if d.Rule[i] < 0 {
		return nil
	}
	return d.Set.Rules[d.Rule[i]]
}
