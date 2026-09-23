package rules

import (
	"fmt"
	"math"
)

// Lint looks for rules that are legal but almost certainly not what the
// author meant, and returns them as warnings. The rules must have passed
// Check. It finds:
//
//   - conditions that are constant once folded ("block if 1 < 2"), which
//     match every payment or none;
//   - and-chains that compare one attribute with constants in ways no value
//     can satisfy (:amount: > 500 and :amount: < 100), usually an or typed
//     as and, or a flipped comparison;
//   - duplicate rules, which share an ID and double-count in reports.
//
// Lint is deliberately shallow: it reasons only about a bare attribute
// compared with a constant inside one and-chain, so it has no false
// positives. A comparison with a missing value is UNKNOWN, which makes the
// and-chain UNKNOWN or FALSE, never TRUE, so a contradiction found this
// way holds for missing values too.
func Lint(rules []*Rule) Diagnostics {
	var ds Diagnostics
	seen := make(map[string]*Rule)
	for _, r := range rules {
		warn := func(sp Span, hint, format string, args ...any) {
			ds = append(ds, Diagnostic{Severity: SeverityWarning, Span: sp, Line: r.line(), Msg: fmt.Sprintf(format, args...), Hint: hint})
		}
		canon := r.String()
		if prev, dup := seen[canon]; dup {
			warn(r.Sp, "Delete one of them.", "this rule is the same as the rule on line %d.", prev.Sp.Start.Line)
		} else {
			seen[canon] = r
		}
		if v, ok := constantCondition(r.Cond); ok {
			const hint = "Check the condition; as written it does not depend on the payment."
			if v {
				warn(r.Cond.Span(), hint, "this condition is always true, so this rule will %s every payment.", r.Action)
			} else {
				warn(r.Cond.Span(), hint, "this condition is never true, so this rule will never match.")
			}
			continue
		}
		if a, b, ok := contradiction(r.Cond); ok {
			warn(Span{a.Span().Start, b.Span().End}, "Check the comparisons, or use or if either one should match.",
				"this condition can never be true: %s and %s cannot both hold.", describe(a), describe(b))
		}
	}
	return ds
}

// constantCondition folds e and reports its value if it is constant.
func constantCondition(e Expr) (v, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			if _, isCE := p.(compileError); !isCE {
				panic(p)
			}
			v, ok = false, false
		}
	}()
	b := compileBool(e, true)
	return b.v, b.konst
}

// constNumber returns the value of a number literal, possibly negated.
func constNumber(e Expr) (float64, bool) {
	switch e := e.(type) {
	case *NumberLit:
		return e.Value, true
	case *Unary:
		if n, ok := e.X.(*NumberLit); ok && e.Op == OpNeg {
			return -n.Value, true
		}
	}
	return 0, false
}

// conjuncts flattens nested ands.
func conjuncts(e Expr, out []Expr) []Expr {
	if b, ok := e.(*Binary); ok && b.Op == OpAnd {
		return conjuncts(b.Y, conjuncts(b.X, out))
	}
	return append(out, e)
}

// bound is one end of a numeric interval and the comparison that set it.
type bound struct {
	v    float64
	incl bool
	by   Expr
}

// numFacts is what an and-chain has said so far about one number attribute.
type numFacts struct {
	lo, hi bound
	ne     []bound
}

// strFacts is the same for a string attribute.
type strFacts struct {
	eq   string
	eqBy Expr
	ne   map[string]Expr
}

// contradiction finds two comparisons in e's top-level and-chain that no
// value of their attribute can satisfy together, returned in source order.
func contradiction(e Expr) (Expr, Expr, bool) {
	nums := make(map[string]*numFacts)
	strs := make(map[string]*strFacts)
	for _, c := range conjuncts(e, nil) {
		b, ok := c.(*Binary)
		if !ok || !b.Op.IsComparison() {
			continue
		}
		op, attr, other := b.Op, b.X, b.Y
		if _, isAttr := attr.(*Attr); !isAttr {
			op, attr, other = flip(op), b.Y, b.X
		}
		a, isAttr := attr.(*Attr)
		if !isAttr {
			continue
		}
		if s, isStr := other.(*StringLit); isStr {
			f := strs[a.Name]
			if f == nil {
				f = &strFacts{ne: make(map[string]Expr)}
				strs[a.Name] = f
			}
			if x, y, bad := f.add(op, s.Value, c); bad {
				return ordered(x, y)
			}
			continue
		}
		if v, isNum := constNumber(other); isNum {
			f := nums[a.Name]
			if f == nil {
				f = &numFacts{lo: bound{v: math.Inf(-1), incl: true}, hi: bound{v: math.Inf(1), incl: true}}
				nums[a.Name] = f
			}
			if x, y, bad := f.add(op, v, c); bad {
				return ordered(x, y)
			}
		}
	}
	return nil, nil, false
}

func (f *strFacts) add(op Op, v string, by Expr) (Expr, Expr, bool) {
	switch op {
	case OpEq:
		if f.eqBy != nil && f.eq != v {
			return f.eqBy, by, true
		}
		if prev, ok := f.ne[v]; ok {
			return prev, by, true
		}
		f.eq, f.eqBy = v, by
	case OpNe:
		if f.eqBy != nil && f.eq == v {
			return f.eqBy, by, true
		}
		f.ne[v] = by
	}
	return nil, nil, false
}

func (f *numFacts) add(op Op, v float64, by Expr) (Expr, Expr, bool) {
	raiseLo := func(incl bool) {
		if v > f.lo.v || v == f.lo.v && f.lo.incl && !incl {
			f.lo = bound{v, incl, by}
		}
	}
	lowerHi := func(incl bool) {
		if v < f.hi.v || v == f.hi.v && f.hi.incl && !incl {
			f.hi = bound{v, incl, by}
		}
	}
	switch op {
	case OpGt, OpGe:
		raiseLo(op == OpGe)
	case OpLt, OpLe:
		lowerHi(op == OpLe)
	case OpEq:
		raiseLo(true)
		lowerHi(true)
	case OpNe:
		f.ne = append(f.ne, bound{v: v, by: by})
	}
	if f.lo.v > f.hi.v || f.lo.v == f.hi.v && !(f.lo.incl && f.hi.incl) {
		return f.lo.by, f.hi.by, true
	}
	if f.lo.v == f.hi.v {
		for _, n := range f.ne {
			if n.v == f.lo.v {
				if n.by == by {
					return f.lo.by, by, true
				}
				return n.by, by, true
			}
		}
	}
	return nil, nil, false
}

func ordered(a, b Expr) (Expr, Expr, bool) {
	if b.Span().Start.Offset < a.Span().Start.Offset {
		a, b = b, a
	}
	return a, b, true
}
