package backtest

import (
	"math/bits"

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
//
// Each worker decides a chunk right after evaluating every rule over it,
// while the chunk's masks are still in cache, so the decision step runs in
// parallel with the rest instead of as a pass over whole bitmaps afterwards.
func EvaluateRuleSet(rs *rules.RuleSet, t *Table, workers int) (*Decisions, error) {
	progs, err := CompileRuleSet(rs, t)
	if err != nil {
		return nil, err
	}
	n := t.N
	d := &Decisions{
		Set:      rs,
		N:        n,
		Action:   make([]rules.Action, n),
		Rule:     make([]int32, n),
		Masks:    make([]*Bitmap, len(progs)),
		Allowed:  NewBitmap(n),
		Blocked:  NewBitmap(n),
		Reviewed: NewBitmap(n),
	}
	nf, nw := 0, 0
	for i, p := range progs {
		d.Masks[i] = NewBitmap(n)
		nf, nw = max(nf, p.nf), max(nw, p.nw)
	}
	// The rules that decide, in decision order: allow, block, review, and
	// source order within each. Shadow rules only fill their masks.
	var order []int
	for _, a := range [3]rules.Action{rules.Allow, rules.Block, rules.Review} {
		for i, r := range rs.Rules {
			if !r.Shadow && r.Action == a {
				order = append(order, i)
			}
		}
	}
	into := map[rules.Action]*Bitmap{rules.Allow: d.Allowed, rules.Block: d.Blocked, rules.Review: d.Reviewed}
	forChunks(n, workers, nf, nw+1, func(c *chunk) {
		w0, nwords := c.lo>>6, words(c.n)
		for i, p := range progs {
			p.root.eval(c, d.Masks[i].W[w0:w0+nwords])
		}
		for i := range c.n {
			d.Action[c.lo+i] = rules.Allow
			d.Rule[c.lo+i] = -1
		}
		// open holds the chunk's rows no rule has decided yet. Deciding in
		// order makes "still open" exactly Radar's eligibility for the next
		// action, and within an action it makes the first rule win.
		open := c.s.w[nw][:nwords]
		for k := range open {
			open[k] = ^uint64(0)
		}
		open[nwords-1] &= tailMask(c.n)
		for _, i := range order {
			r := rs.Rules[i]
			mask, dst := d.Masks[i].W[w0:w0+nwords], into[r.Action].W[w0:w0+nwords]
			for k, m := range mask {
				newly := m & open[k]
				if newly == 0 {
					continue
				}
				open[k] &^= newly
				dst[k] |= newly
				for x := newly; x != 0; x &= x - 1 {
					row := c.lo + k<<6 | bits.TrailingZeros64(x)
					d.Action[row] = r.Action
					d.Rule[row] = int32(i)
				}
			}
		}
	})
	return d, nil
}

// DecidingRule returns the rule that decided row i, or nil for the default.
func (d *Decisions) DecidingRule(i int) *rules.CompiledRule {
	if d.Rule[i] < 0 {
		return nil
	}
	return d.Set.Rules[d.Rule[i]]
}
