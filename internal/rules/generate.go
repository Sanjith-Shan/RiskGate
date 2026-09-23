package rules

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// This file generates random well-typed rules and matching random rows. It
// exists for differential testing: the closure compiler here and the
// columnar evaluator in the backtester must agree on every generated rule
// and every row. Constants and row values come from the same small pools
// per field, so equality, list membership and boundary comparisons actually
// hit, and rows leave fields missing often enough to exercise the missing
// semantics on every operator.

// GenerateExpr returns a random well-typed condition over env, with at most
// depth levels of nesting below the root. The tree is unresolved and has no
// spans; print it, or use Generate for a parsed and checked rule.
func GenerateExpr(rng *rand.Rand, env Env, depth int) Expr {
	return newGen(rng, env).cond(depth)
}

// Generate returns a random well-typed rule, possibly shadow, produced by
// GenerateExpr, printed, and parsed and checked again, so that its spans,
// Text and resolved fields are exactly what Load would produce from the
// same text. It panics if the generated text fails to parse or check, which
// would be a bug in the generator, the printer or the checker.
func Generate(rng *rand.Rand, env Env, depth int) *Rule {
	g := newGen(rng, env)
	r := &Rule{
		Action: Action(1 + rng.IntN(3)),
		Shadow: rng.IntN(10) == 0,
		Cond:   g.cond(depth),
	}
	text := r.String()
	parsed, err := ParseRule(text)
	if err != nil {
		panic(fmt.Sprintf("rules: generated rule does not parse: %s\n%v", text, err))
	}
	if err := Check([]*Rule{parsed}, env); err != nil {
		panic(fmt.Sprintf("rules: generated rule does not check: %s\n%v", text, err))
	}
	return parsed
}

// GenerateRow returns a random row for cat. Each field is missing with
// probability 1/5; other values come from the pools Generate draws its
// constants from.
func GenerateRow(rng *rand.Rand, cat *schema.Catalog) schema.Row {
	row := cat.NewRow()
	for _, f := range cat.Fields() {
		if rng.IntN(5) == 0 {
			continue // leave missing
		}
		switch f.Kind {
		case schema.Number:
			row.Num[f.Slot] = numValue(rng, f.Name)
		case schema.String:
			row.Str[f.Slot] = strValue(rng, f.Name)
		}
	}
	return row
}

// SampleLists returns named lists drawn from the generator's value pools,
// for tests, examples and demos.
func SampleLists() Lists {
	return Lists{
		"trusted_domains":    StringList("gmail.com", "icloud.com", "outlook.com"),
		"risky_domains":      StringList("anonymous.com", "protonmail.com", "mail.ru", "guerrillamail.com", "yopmail.com", "10minutemail.com", "tempmail.net", "throwaway.email", "sharklasers.com"),
		"prepaid_networks":   StringList("discover", "american express"),
		"blocked_regions":    NumberList(123, 204, 299, 325, 441),
		"watched_countries":  NumberList(16, 31, 60, 65, 87, 96, 102, 13, 29, 32),
		"suspicious_devices": StringList("SM-G892A Build/NRD90M", "rv:11.0", "Trident/7.0"),
	}
}

type gen struct {
	rng                *rand.Rand
	env                Env
	nums, strs         []schema.Field
	numLists, strLists []string
}

func newGen(rng *rand.Rand, env Env) *gen {
	g := &gen{rng: rng, env: env}
	for _, f := range env.Catalog.Fields() {
		if f.Kind == schema.Number {
			g.nums = append(g.nums, f)
		} else {
			g.strs = append(g.strs, f)
		}
	}
	g.numLists = env.listNames(schema.Number)
	g.strLists = env.listNames(schema.String)
	return g
}

func (g *gen) chance(n int) bool { return g.rng.IntN(n) == 0 }

func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.IntN(len(xs))] }

// cond returns a condition. At depth 0 it is a leaf comparison; above that
// it may combine smaller conditions and use compound operands.
func (g *gen) cond(depth int) Expr {
	if depth <= 0 {
		return g.leafCond()
	}
	switch g.rng.IntN(10) {
	case 0, 1:
		return &Binary{Op: OpAnd, X: g.cond(depth - 1), Y: g.cond(depth - 1)}
	case 2, 3:
		return &Binary{Op: OpOr, X: g.cond(depth - 1), Y: g.cond(depth - 1)}
	case 4:
		return &Unary{Op: OpNot, X: g.cond(depth - 1)}
	case 5:
		return &Binary{Op: pick(g.rng, cmpOps), X: g.num(depth - 1), Y: g.num(depth - 1)}
	case 6:
		if len(g.strs) > 0 {
			return &Binary{Op: pick(g.rng, []Op{OpEq, OpNe}), X: g.str(depth - 1), Y: g.str(depth - 1)}
		}
	case 7:
		return g.in(depth - 1)
	case 8:
		if g.chance(2) || len(g.strs) == 0 {
			return &Call{Name: "is_missing", Args: []Expr{g.num(depth - 1)}}
		}
		return &Call{Name: "is_missing", Args: []Expr{g.str(depth - 1)}}
	case 9:
		if len(g.strs) > 0 {
			return &Call{Name: "starts_with", Args: []Expr{g.str(depth - 1), g.prefix(depth - 1)}}
		}
	}
	return g.leafCond()
}

var cmpOps = []Op{OpEq, OpNe, OpLt, OpLe, OpGt, OpGe}

// leafCond is the shape real rules are made of: one attribute against a
// realistic constant, a missing test, or a list test.
func (g *gen) leafCond() Expr {
	switch n := g.rng.IntN(20); {
	case n < 9 || len(g.strs) == 0 && n < 14:
		f := pick(g.rng, g.nums)
		var x, y Expr = &Attr{Name: f.Name}, g.numConst(f.Name)
		if g.chance(4) {
			x, y = y, x
		}
		return &Binary{Op: pick(g.rng, cmpOps), X: x, Y: y}
	case n < 13 && len(g.strs) > 0:
		f := pick(g.rng, g.strs)
		var x Expr = &Attr{Name: f.Name}
		if g.chance(3) {
			x = &Call{Name: "lower", Args: []Expr{x}}
		}
		return &Binary{Op: pick(g.rng, []Op{OpEq, OpNe}), X: x, Y: &StringLit{Value: strValue(g.rng, f.Name)}}
	case n < 16:
		return g.in(0)
	case n < 18:
		return &Call{Name: "is_missing", Args: []Expr{g.attr()}}
	case n < 19 && len(g.strs) > 0:
		return &Call{Name: "starts_with", Args: []Expr{g.str(0), g.prefix(0)}}
	}
	return &BoolLit{Value: g.chance(2)}
}

func (g *gen) attr() Expr {
	if len(g.strs) > 0 && g.chance(3) {
		return &Attr{Name: pick(g.rng, g.strs).Name}
	}
	return &Attr{Name: pick(g.rng, g.nums).Name}
}

// num returns a number expression.
func (g *gen) num(depth int) Expr {
	if depth > 0 && g.chance(2) {
		if g.chance(6) {
			return &Unary{Op: OpNeg, X: g.num(depth - 1)}
		}
		op := pick(g.rng, []Op{OpAdd, OpSub, OpMul, OpDiv})
		return &Binary{Op: op, X: g.num(depth - 1), Y: g.num(depth - 1)}
	}
	f := pick(g.rng, g.nums)
	if g.chance(3) {
		return g.numConst(f.Name)
	}
	return &Attr{Name: f.Name}
}

// numConst returns a literal from field's pool, occasionally negative and
// occasionally zero so division by zero comes up.
func (g *gen) numConst(field string) Expr {
	v := numValue(g.rng, field)
	if g.chance(12) {
		v = 0
	}
	lit := &NumberLit{Value: v}
	if g.chance(10) {
		return &Unary{Op: OpNeg, X: lit}
	}
	return lit
}

// str returns a string expression.
func (g *gen) str(depth int) Expr {
	f := pick(g.rng, g.strs)
	var e Expr = &Attr{Name: f.Name}
	if g.chance(4) {
		e = &StringLit{Value: strValue(g.rng, f.Name)}
	}
	if depth > 0 && g.chance(3) {
		e = &Call{Name: "lower", Args: []Expr{g.str(depth - 1)}}
	}
	return e
}

// prefix returns a string expression suitable as a starts_with prefix:
// usually a short literal cut from a pool value, so it matches sometimes.
func (g *gen) prefix(depth int) Expr {
	if g.chance(4) {
		return g.str(depth)
	}
	v := strValue(g.rng, pick(g.rng, g.strs).Name)
	r := []rune(v)
	return &StringLit{Value: string(r[:1+g.rng.IntN(len(r))])}
}

func (g *gen) in(depth int) Expr {
	useStr := len(g.strs) > 0 && g.chance(2)
	if useStr {
		x := g.str(depth)
		if lists := g.strLists; len(lists) > 0 && g.chance(2) {
			return &In{X: x, List: &ListRef{Name: pick(g.rng, lists)}}
		}
		f := pick(g.rng, g.strs).Name
		if a, ok := x.(*Attr); ok {
			f = a.Name
		}
		items := make([]Expr, 1+g.rng.IntN(linearMax+4))
		for i := range items {
			items[i] = &StringLit{Value: strValue(g.rng, f)}
		}
		return &In{X: x, List: &ListLit{Items: items}}
	}
	x := g.num(depth)
	if lists := g.numLists; len(lists) > 0 && g.chance(2) {
		return &In{X: x, List: &ListRef{Name: pick(g.rng, lists)}}
	}
	f := pick(g.rng, g.nums).Name
	if a, ok := x.(*Attr); ok {
		f = a.Name
	}
	items := make([]Expr, 1+g.rng.IntN(linearMax+4))
	for i := range items {
		items[i] = g.numConst(f)
	}
	return &In{X: x, List: &ListLit{Items: items}}
}

// numValue draws a realistic value for a number field, by the naming
// conventions of schema.VelocityFieldNames. Integers where the field counts
// things, cents where it is money.
func numValue(rng *rand.Rand, field string) float64 {
	var landmarks []float64
	var lo, hi float64
	cents := false
	switch {
	case strings.Contains(field, "count") || strings.HasPrefix(field, "distinct"):
		lo, hi, landmarks = 0, 30, []float64{0, 1, 2, 3, 5, 8, 10}
	case strings.Contains(field, "ratio"):
		lo, hi, landmarks, cents = 0, 12, []float64{0.5, 1, 2, 3, 5}, true
	case strings.Contains(field, "seconds"):
		lo, hi, landmarks = 0, 7*86400, []float64{0, 60, 3600, 86400}
	case field == "risk_score":
		lo, hi, landmarks = 0, 99, []float64{0, 50, 75, 85, 90, 99}
	case field == "billing_region":
		lo, hi, landmarks = 100, 540, []float64{123, 204, 299, 325, 441}
	case field == "billing_country_code":
		lo, hi, landmarks = 10, 102, []float64{16, 31, 60, 87, 96}
	case field == "distance":
		lo, hi, landmarks = 0, 2000, []float64{0, 1, 10, 100}
	case strings.Contains(field, "amount") || strings.Contains(field, "sum"):
		lo, hi, landmarks, cents = 0, 5000, []float64{0, 25, 49.99, 100, 117.5, 300, 1000}, true
	default:
		lo, hi, landmarks = 0, 100, []float64{0, 1, 10, 50}
	}
	if rng.IntN(3) == 0 {
		return pick(rng, landmarks)
	}
	v := lo + rng.Float64()*(hi-lo)
	if cents {
		return math.Round(v*100) / 100
	}
	return math.Round(v)
}

// String pools. Mixed case and a little non-ASCII so that lower() and
// equality are tested on text that lowering actually changes.
var stringPools = map[string][]string{
	"product_code": {"W", "C", "R", "H", "S", "w"},
	"card_network": {"visa", "mastercard", "american express", "discover", "Visa", "MASTERCARD"},
	"card_type":    {"debit", "credit", "charge card", "debit or credit", "Credit"},
	"device_type":  {"desktop", "mobile", "Mobile"},
	"device_info": {"Windows", "iOS Device", "MacOS", "Trident/7.0", "rv:11.0",
		"SM-G892A Build/NRD90M", "SM-J700M Build/MMB29K", "Moto G (4) Build/NPJ25.93-14", "linux"},
}

var emailPool = []string{"gmail.com", "Gmail.com", "GMAIL.COM", "yahoo.com", "hotmail.com", "anonymous.com",
	"outlook.com", "icloud.com", "protonmail.com", "mail.ru", "aol.com", "ÉCOLE.fr", "straße.de", "gmail"}

func strValue(rng *rand.Rand, field string) string {
	if pool, ok := stringPools[field]; ok {
		return pick(rng, pool)
	}
	if strings.Contains(field, "email") {
		return pick(rng, emailPool)
	}
	all := slices.Concat(emailPool, stringPools["device_info"])
	return pick(rng, all)
}
