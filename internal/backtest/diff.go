package backtest

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// The differential test (experiment 3): the closure evaluator and the
// vectorized evaluator are two independent implementations of one
// specification, so on any rule and any row they must agree. Random
// well-typed rules from rules.Generate, over tables with heavy missingness
// and adversarial values, find the places where one of them read the
// specification differently.

// DiffOptions configures DiffTest.
type DiffOptions struct {
	Rules    int    // rules to generate
	Seed     uint64 // generator seed
	MinDepth int    // nesting depth cycles through [MinDepth, MaxDepth]
	MaxDepth int
	// Ground replaces the generator's constants with values drawn from the
	// table itself, so that on real data equality and list tests hit real
	// values, not only the generator's pools.
	Ground bool
	// Workers evaluates rules in parallel; 0 means GOMAXPROCS.
	Workers int
	// MaxExamples bounds the disagreements kept for the report.
	MaxExamples int
}

// Disagreement is one rule and row on which the evaluators differ.
type Disagreement struct {
	Rule    string `json:"rule"`
	Row     int    `json:"row"`
	ID      int64  `json:"id"`
	Closure bool   `json:"closure"`
	Vector  bool   `json:"vector"`
}

// DiffResult summarizes a differential run.
type DiffResult struct {
	Rules         int            `json:"rules"`
	Rows          int            `json:"rows"`
	Checked       int64          `json:"checked"`  // rule-row pairs compared
	Matched       int64          `json:"matched"`  // pairs where both said true
	Trivial       int            `json:"trivial"`  // rules matching no row or every row
	Grounded      int            `json:"grounded"` // rules whose constants came from the table
	Disagreements int64          `json:"disagreements"`
	BadRules      int            `json:"bad_rules"` // rules with at least one disagreement
	Examples      []Disagreement `json:"examples,omitempty"`
}

// GenerateRules returns n generated, checked rules, with depths cycling
// through [minDepth, maxDepth], optionally grounded in t.
func GenerateRules(env rules.Env, t *Table, n int, seed uint64, minDepth, maxDepth int, ground bool) (rs []*rules.Rule, grounded int) {
	rng := rand.New(rand.NewPCG(seed, 0xd1ff))
	minDepth = max(minDepth, 0)
	maxDepth = max(maxDepth, minDepth)
	for i := range n {
		r := rules.Generate(rng, env, minDepth+i%(maxDepth-minDepth+1))
		if ground && t != nil && t.N > 0 {
			if g, ok := Ground(r, t, rng, env); ok {
				r = g
				grounded++
			}
		}
		rs = append(rs, r)
	}
	return rs, grounded
}

// DiffTest generates rules as DiffOptions says and checks both evaluators on
// every row of t.
func DiffTest(t *Table, env rules.Env, opt DiffOptions) (DiffResult, error) {
	rs, grounded := GenerateRules(env, t, opt.Rules, opt.Seed, opt.MinDepth, opt.MaxDepth, opt.Ground)
	res, err := DiffRules(t, t.Rows(), rs, opt.Workers, opt.MaxExamples)
	res.Grounded = grounded
	return res, err
}

// DiffRules checks the closure and vectorized evaluators against each other
// for every rule on every row. rows must be t.Rows().
func DiffRules(t *Table, rows []schema.Row, rs []*rules.Rule, workers, maxExamples int) (DiffResult, error) {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if maxExamples <= 0 {
		maxExamples = 20
	}
	res := DiffResult{Rules: len(rs), Rows: t.N}
	var (
		mu       sync.Mutex
		firstErr error
		next     atomic.Int64
		wg       sync.WaitGroup
	)
	for range min(workers, max(len(rs), 1)) {
		wg.Go(func() {
			for {
				k := int(next.Add(1) - 1)
				if k >= len(rs) {
					return
				}
				r := rs[k]
				cr, err := rules.Compile(r)
				if err == nil {
					var p *Program
					if p, err = CompileVector(r.Cond, t); err == nil {
						vec := p.EvalWorkers(1)
						clo := MatchRows(cr, rows)
						matched := vec.AndCount(clo)
						var bad []int
						for w := range vec.W {
							for x := vec.W[w] ^ clo.W[w]; x != 0; x &= x - 1 {
								bad = append(bad, w<<6|bits.TrailingZeros64(x))
							}
						}
						mu.Lock()
						res.Checked += int64(t.N)
						res.Matched += int64(matched)
						if n := vec.Count(); n == 0 || n == t.N {
							res.Trivial++
						}
						if len(bad) > 0 {
							res.BadRules++
							res.Disagreements += int64(len(bad))
							for _, row := range bad[:min(len(bad), 3)] {
								if len(res.Examples) < maxExamples {
									res.Examples = append(res.Examples, Disagreement{
										Rule: r.String(), Row: row, ID: t.ID[row],
										Closure: clo.Get(row), Vector: vec.Get(row),
									})
								}
							}
						}
						mu.Unlock()
						continue
					}
				}
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("rule %q: %w", r.String(), err)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return res, firstErr
}

// Ground returns a copy of r whose constants are replaced by values drawn
// from t: each literal compared with, or listed against, an attribute takes
// a present value of that attribute from a random row. The result is
// printed, parsed and checked again, so it is exactly what Load would build
// from its text. ok is false when nothing was replaced or the result does
// not check (for example a value the language cannot write as a literal).
func Ground(r *rules.Rule, t *Table, rng *rand.Rand, env rules.Env) (g *rules.Rule, ok bool) {
	// Work on a fresh parse, so r itself is never modified.
	cp, err := rules.ParseRule(r.String())
	if err != nil {
		return nil, false
	}
	changed := false
	sampleNum := func(slot int) (float64, bool) {
		col := t.Num[slot]
		for range 8 {
			if v := col[rng.IntN(t.N)]; !math.IsNaN(v) && !math.IsInf(v, 0) {
				return v, true
			}
		}
		return 0, false
	}
	sampleStr := func(slot int) (string, bool) {
		if d := t.Dict[slot]; len(d) > 1 {
			// Draw by row, not by dictionary entry, so common values come
			// up as often as they occur.
			for range 8 {
				if c := t.Str[slot][rng.IntN(t.N)]; c != 0 {
					return d[c], true
				}
			}
			return d[1+rng.IntN(len(d)-1)], true
		}
		return "", false
	}
	setLit := func(lit rules.Expr, attr *rules.Attr) {
		f, known := env.Catalog.Lookup(attr.Name)
		if !known {
			return
		}
		switch l := lit.(type) {
		case *rules.NumberLit:
			if f.Kind == schema.Number {
				if v, ok := sampleNum(f.Slot); ok {
					l.Value, changed = math.Abs(v), true // literals are non-negative; a sign is a Unary
				}
			}
		case *rules.StringLit:
			if f.Kind == schema.String {
				if v, ok := sampleStr(f.Slot); ok {
					l.Value, changed = v, true
				}
			}
		}
	}
	// attrOf sees through lower(), so lower(:x:) = "..." is grounded in
	// :x:'s values too (half the time lowered, so it can match).
	attrOf := func(e rules.Expr) (*rules.Attr, bool) {
		switch e := e.(type) {
		case *rules.Attr:
			return e, false
		case *rules.Call:
			if e.Name == "lower" && len(e.Args) == 1 { // cp is parsed, not checked: no Fn yet
				if a, ok := e.Args[0].(*rules.Attr); ok {
					return a, true
				}
			}
		}
		return nil, false
	}
	ground := func(lit, other rules.Expr) {
		a, lowered := attrOf(other)
		if a == nil {
			return
		}
		setLit(lit, a)
		if s, ok := lit.(*rules.StringLit); ok && lowered && rng.IntN(2) == 0 {
			s.Value = strings.ToLower(s.Value)
		}
	}
	rules.Inspect(cp.Cond, func(e rules.Expr) bool {
		switch e := e.(type) {
		case *rules.Binary:
			if e.Op.IsComparison() {
				ground(e.Y, e.X)
				ground(e.X, e.Y)
			}
		case *rules.In:
			if a, isAttr := e.X.(*rules.Attr); isAttr {
				if l, isLit := e.List.(*rules.ListLit); isLit {
					for _, it := range l.Items {
						setLit(it, a)
					}
				}
			}
		}
		return true
	})
	if !changed {
		return nil, false
	}
	g, err = rules.ParseRule(cp.String())
	if err != nil {
		return nil, false
	}
	if rules.Check([]*rules.Rule{g}, env) != nil {
		return nil, false
	}
	return g, true
}
