package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// CompiledRule is a checked rule turned into a tree of Go closures over
// schema.Row. Match does no allocation, reflection or interface dispatch.
type CompiledRule struct {
	// ID is derived from the rule's canonical text, so it survives
	// reformatting and reordering: the same rule has the same ID in every
	// version of the rule set. Identical rules share an ID (Lint warns).
	ID     string
	Index  int    // position in the rule set, from 0
	Line   int    // source line, from 1
	Text   string // as written
	Action Action
	Shadow bool
	Rule   *Rule // the checked AST

	match func(schema.Row) bool
}

// Match reports whether the rule's condition holds for row. The row must have
// been built for the catalog the rule was checked against.
func (c *CompiledRule) Match(row schema.Row) bool { return c.match(row) }

// RuleID returns the stable ID of a rule: a hash of its canonical text.
func RuleID(r *Rule) string {
	sum := sha256.Sum256([]byte(r.String()))
	return "r_" + hex.EncodeToString(sum[:6])
}

// compileError is raised when a tree has not been through Check, or was
// changed after it. It is recovered in Compile.
type compileError struct{ msg string }

func bad(format string, args ...any) { panic(compileError{fmt.Sprintf(format, args...)}) }

// Compile turns a rule that has passed Check into closures.
//
// Sub-expressions compile to typed closures: numbers to func(Row) float64
// with NaN for missing, strings to func(Row) string with "" for missing, and
// conditions to func(Row) bool. Constant sub-trees are folded at compile
// time by running the very closure that would have run per row, once, so
// folding can never disagree with evaluation. Comparisons of an attribute
// against a constant, the commonest shape by far, get specialized closures
// that read the row directly.
func Compile(r *Rule) (cr *CompiledRule, err error) {
	defer func() {
		if p := recover(); p != nil {
			ce, ok := p.(compileError)
			if !ok {
				panic(p)
			}
			err = fmt.Errorf("rules: cannot compile %q: %s (was it checked?)", r.Text, ce.msg)
		}
	}()
	b := compileBool(r.Cond)
	return &CompiledRule{
		ID:     RuleID(r),
		Index:  r.Index,
		Line:   r.Sp.Start.Line,
		Text:   r.Text,
		Action: r.Action,
		Shadow: r.Shadow,
		Rule:   r,
		match:  b.fn,
	}, nil
}

// CompileExpr compiles a checked condition on its own.
func CompileExpr(e Expr) (fn func(schema.Row) bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			ce, ok := p.(compileError)
			if !ok {
				panic(p)
			}
			err = fmt.Errorf("rules: cannot compile %s: %s (was it checked?)", Print(e), ce.msg)
		}
	}()
	return compileBool(e).fn, nil
}

var nan = math.NaN()

// empty is the row constant folding evaluates against. A constant closure
// never reads it.
var empty schema.Row

type numC struct {
	fn    func(schema.Row) float64
	konst bool
	v     float64
	slot  int // row slot when the expression is a bare attribute, else -1
}

type strC struct {
	fn    func(schema.Row) string // the value before lowering
	lower bool                    // compare as strings.ToLower(fn(row))
	konst bool
	v     string // constant value, already lowered
	slot  int
}

type boolC struct {
	fn    func(schema.Row) bool
	konst bool
	v     bool
}

func numConst(v float64) numC {
	return numC{fn: func(schema.Row) float64 { return v }, konst: true, v: v, slot: -1}
}

func strConst(v string) strC {
	return strC{fn: func(schema.Row) string { return v }, konst: true, v: v, slot: -1}
}

func boolConst(v bool) boolC {
	return boolC{fn: func(schema.Row) bool { return v }, konst: true, v: v}
}

// foldBool evaluates fn once if all its inputs are constant.
func foldBool(fn func(schema.Row) bool, allConst bool) boolC {
	if allConst {
		return boolConst(fn(empty))
	}
	return boolC{fn: fn}
}

func compileNum(e Expr) numC {
	switch e := e.(type) {
	case *Attr:
		if e.Field.Kind != schema.Number {
			bad(":%s: is not resolved to a number", e.Name)
		}
		s := e.Field.Slot
		return numC{fn: func(r schema.Row) float64 { return r.Num[s] }, slot: s}
	case *NumberLit:
		return numConst(e.Value)
	case *Unary:
		if e.Op != OpNeg {
			bad("%s is not a number", describe(e))
		}
		x := compileNum(e.X)
		if x.konst {
			return numConst(-x.v)
		}
		xf := x.fn
		return numC{fn: func(r schema.Row) float64 { return -xf(r) }, slot: -1}
	case *Binary:
		if !e.Op.IsArithmetic() {
			bad("%s is not a number", describe(e))
		}
		x, y := compileNum(e.X), compileNum(e.Y)
		fn := arith(e.Op, x, y)
		if x.konst && y.konst {
			return numConst(fn(empty))
		}
		if y.konst && e.Op == OpDiv && y.v == 0 {
			return numConst(nan) // dividing by a constant zero is always missing
		}
		return numC{fn: fn, slot: -1}
	}
	bad("%s is not a number", describe(e))
	return numC{}
}

// arith builds x op y. Division by zero yields NaN (missing) rather than
// ±Inf: a ratio over an empty history is unknown, not infinite. NaN operands
// propagate through IEEE arithmetic unaided.
func arith(op Op, x, y numC) func(schema.Row) float64 {
	xf, yf := x.fn, y.fn
	if y.konst && !x.konst {
		c := y.v
		switch op {
		case OpAdd:
			return func(r schema.Row) float64 { return xf(r) + c }
		case OpSub:
			return func(r schema.Row) float64 { return xf(r) - c }
		case OpMul:
			return func(r schema.Row) float64 { return xf(r) * c }
		case OpDiv:
			return func(r schema.Row) float64 { return div(xf(r), c) }
		}
	}
	if x.konst && !y.konst {
		c := x.v
		switch op {
		case OpAdd:
			return func(r schema.Row) float64 { return c + yf(r) }
		case OpSub:
			return func(r schema.Row) float64 { return c - yf(r) }
		case OpMul:
			return func(r schema.Row) float64 { return c * yf(r) }
		case OpDiv:
			return func(r schema.Row) float64 { return div(c, yf(r)) }
		}
	}
	switch op {
	case OpAdd:
		return func(r schema.Row) float64 { return xf(r) + yf(r) }
	case OpSub:
		return func(r schema.Row) float64 { return xf(r) - yf(r) }
	case OpMul:
		return func(r schema.Row) float64 { return xf(r) * yf(r) }
	case OpDiv:
		return func(r schema.Row) float64 { return div(xf(r), yf(r)) }
	}
	bad("unknown arithmetic operator %s", op)
	return nil
}

func div(a, b float64) float64 {
	if b == 0 {
		return nan
	}
	return a / b
}

func compileStr(e Expr) strC {
	switch e := e.(type) {
	case *Attr:
		if e.Field.Kind != schema.String {
			bad(":%s: is not resolved to a string", e.Name)
		}
		s := e.Field.Slot
		return strC{fn: func(r schema.Row) string { return r.Str[s] }, slot: s}
	case *StringLit:
		return strConst(e.Value)
	case *Call:
		if e.Fn != FuncLower || len(e.Args) != 1 {
			bad("%s is not text", describe(e))
		}
		x := compileStr(e.Args[0])
		if x.konst {
			return strConst(strings.ToLower(x.v))
		}
		x.lower = true // idempotent, so lower(lower(x)) is lower(x)
		return x
	}
	bad("%s is not text", describe(e))
	return strC{}
}

func compileBool(e Expr) boolC {
	switch e := e.(type) {
	case *BoolLit:
		return boolConst(e.Value)
	case *Unary:
		if e.Op != OpNot {
			bad("%s is not a condition", describe(e))
		}
		x := compileBool(e.X)
		if x.konst {
			return boolConst(!x.v)
		}
		xf := x.fn
		return boolC{fn: func(r schema.Row) bool { return !xf(r) }}
	case *Binary:
		switch {
		case e.Op == OpAnd || e.Op == OpOr:
			return logical(e.Op, compileBool(e.X), compileBool(e.Y))
		case e.Op.IsComparison():
			switch e.X.Type() {
			case TypeNumber:
				return compareNum(e.Op, compileNum(e.X), compileNum(e.Y))
			case TypeString:
				return compareStr(e.Op, compileStr(e.X), compileStr(e.Y))
			}
		}
	case *In:
		switch e.Kind {
		case schema.Number:
			return inNum(compileNum(e.X), e.Numbers)
		case schema.String:
			return inStr(compileStr(e.X), e.Strings)
		}
	case *Call:
		switch e.Fn {
		case FuncIsMissing:
			if len(e.Args) == 1 {
				return isMissing(e.Args[0])
			}
		case FuncStartsWith:
			if len(e.Args) == 2 {
				return startsWith(compileStr(e.Args[0]), compileStr(e.Args[1]))
			}
		}
	}
	bad("%s is not a condition", describe(e))
	return boolC{}
}

// logical builds and/or. With two-valued logic a constant operand either
// decides the result or drops out, whatever the other side evaluates to.
func logical(op Op, x, y boolC) boolC {
	decisive := op == OpOr // true decides or, false decides and
	for _, pair := range [2][2]boolC{{x, y}, {y, x}} {
		if c, other := pair[0], pair[1]; c.konst {
			if c.v == decisive {
				return boolConst(decisive)
			}
			return other
		}
	}
	xf, yf := x.fn, y.fn
	if op == OpAnd {
		return boolC{fn: func(r schema.Row) bool { return xf(r) && yf(r) }}
	}
	return boolC{fn: func(r schema.Row) bool { return xf(r) || yf(r) }}
}

func flip(op Op) Op {
	switch op {
	case OpLt:
		return OpGt
	case OpLe:
		return OpGe
	case OpGt:
		return OpLt
	case OpGe:
		return OpLe
	}
	return op
}

// compareNum builds a numeric comparison. Go's float comparisons are already
// false when either side is NaN, except !=, which has to rule missing out
// explicitly: "missing != 5" is false like every comparison with missing.
func compareNum(op Op, x, y numC) boolC {
	if x.konst && !y.konst {
		x, y, op = y, x, flip(op)
	}
	if y.konst && !x.konst {
		c := y.v
		if math.IsNaN(c) {
			return boolConst(false)
		}
		if x.slot >= 0 {
			return boolC{fn: compareSlotConst(op, x.slot, c)}
		}
		xf := x.fn
		switch op {
		case OpEq:
			return boolC{fn: func(r schema.Row) bool { return xf(r) == c }}
		case OpNe:
			return boolC{fn: func(r schema.Row) bool { v := xf(r); return v == v && v != c }}
		case OpLt:
			return boolC{fn: func(r schema.Row) bool { return xf(r) < c }}
		case OpLe:
			return boolC{fn: func(r schema.Row) bool { return xf(r) <= c }}
		case OpGt:
			return boolC{fn: func(r schema.Row) bool { return xf(r) > c }}
		case OpGe:
			return boolC{fn: func(r schema.Row) bool { return xf(r) >= c }}
		}
	}
	xf, yf := x.fn, y.fn
	var fn func(schema.Row) bool
	switch op {
	case OpEq:
		fn = func(r schema.Row) bool { return xf(r) == yf(r) }
	case OpNe:
		fn = func(r schema.Row) bool { a, b := xf(r), yf(r); return a == a && b == b && a != b }
	case OpLt:
		fn = func(r schema.Row) bool { return xf(r) < yf(r) }
	case OpLe:
		fn = func(r schema.Row) bool { return xf(r) <= yf(r) }
	case OpGt:
		fn = func(r schema.Row) bool { return xf(r) > yf(r) }
	case OpGe:
		fn = func(r schema.Row) bool { return xf(r) >= yf(r) }
	default:
		bad("unknown comparison %s", op)
	}
	return foldBool(fn, x.konst && y.konst)
}

func compareSlotConst(op Op, s int, c float64) func(schema.Row) bool {
	switch op {
	case OpEq:
		return func(r schema.Row) bool { return r.Num[s] == c }
	case OpNe:
		return func(r schema.Row) bool { v := r.Num[s]; return v == v && v != c }
	case OpLt:
		return func(r schema.Row) bool { return r.Num[s] < c }
	case OpLe:
		return func(r schema.Row) bool { return r.Num[s] <= c }
	case OpGt:
		return func(r schema.Row) bool { return r.Num[s] > c }
	case OpGe:
		return func(r schema.Row) bool { return r.Num[s] >= c }
	}
	bad("unknown comparison %s", op)
	return nil
}

// compareStr builds = or != over text. Either side missing ("") makes the
// comparison false.
func compareStr(op Op, x, y strC) boolC {
	if op != OpEq && op != OpNe {
		bad("%s does not compare text", op)
	}
	ne := op == OpNe
	if x.konst && !y.konst {
		x, y = y, x
	}
	if y.konst && !x.konst {
		c := y.v
		if c == "" {
			return boolConst(false)
		}
		xl := x.lower
		if x.slot >= 0 && !xl {
			s := x.slot
			if ne {
				return boolC{fn: func(r schema.Row) bool { v := r.Str[s]; return v != "" && v != c }}
			}
			return boolC{fn: func(r schema.Row) bool { return r.Str[s] == c }}
		}
		xf := x.fn
		return boolC{fn: func(r schema.Row) bool {
			v := xf(r)
			return v != "" && foldEqual(v, xl, c, false) != ne
		}}
	}
	xf, yf, xl, yl := x.fn, y.fn, x.lower, y.lower
	fn := func(r schema.Row) bool {
		a, b := xf(r), yf(r)
		return a != "" && b != "" && foldEqual(a, xl, b, yl) != ne
	}
	return foldBool(fn, x.konst && y.konst)
}

// linearMax is the list size up to which membership is a linear scan. Below
// it a scan beats hashing; above it a map does.
const linearMax = 8

func inNum(x numC, values []float64) boolC {
	xf := x.fn
	var fn func(schema.Row) bool
	if len(values) <= linearMax {
		fn = func(r schema.Row) bool {
			v := xf(r)
			for _, n := range values {
				if v == n {
					return true
				}
			}
			return false
		}
	} else {
		set := make(map[float64]struct{}, len(values))
		for _, v := range values {
			set[v] = struct{}{}
		}
		// NaN is never a map key match, so missing is never in the set.
		fn = func(r schema.Row) bool { _, ok := set[xf(r)]; return ok }
	}
	return foldBool(fn, x.konst)
}

// lowerBufSize bounds the stack buffer used to look up a lowered value in a
// hashed list. Longer values fall back to a scan.
const lowerBufSize = 128

func inStr(x strC, values []string) boolC {
	if x.konst && x.lower {
		bad("constant lowered text was not folded")
	}
	xf, xl := x.fn, x.lower
	var fn func(schema.Row) bool
	switch {
	case len(values) <= linearMax:
		fn = func(r schema.Row) bool {
			v := xf(r)
			if v == "" {
				return false
			}
			for _, s := range values {
				if foldEqual(v, xl, s, false) {
					return true
				}
			}
			return false
		}
	case !xl:
		set := make(map[string]struct{}, len(values))
		for _, v := range values {
			set[v] = struct{}{}
		}
		fn = func(r schema.Row) bool { _, ok := set[xf(r)]; return ok }
	default:
		set := make(map[string]struct{}, len(values))
		for _, v := range values {
			set[v] = struct{}{}
		}
		fn = func(r schema.Row) bool {
			v := xf(r)
			if v == "" {
				return false
			}
			var buf [lowerBufSize]byte
			if n, ok := lowerInto(buf[:], v); ok {
				_, hit := set[string(buf[:n])] // no allocation: the compiler special-cases map[string(bytes)]
				return hit
			}
			for _, s := range values {
				if foldEqual(v, true, s, false) {
					return true
				}
			}
			return false
		}
	}
	return foldBool(fn, x.konst)
}

func isMissing(arg Expr) boolC {
	switch arg.Type() {
	case TypeNumber:
		x := compileNum(arg)
		if x.slot >= 0 {
			s := x.slot
			return boolC{fn: func(r schema.Row) bool { v := r.Num[s]; return v != v }}
		}
		xf := x.fn
		return foldBool(func(r schema.Row) bool { return math.IsNaN(xf(r)) }, x.konst)
	case TypeString:
		// Lowering never turns non-empty text into empty text, so the
		// unlowered value answers for lower(x) too.
		x := compileStr(arg)
		xf := x.fn
		return foldBool(func(r schema.Row) bool { return xf(r) == "" }, x.konst)
	}
	bad("is_missing of %s", describe(arg))
	return boolC{}
}

func startsWith(x, p strC) boolC {
	xf, pf, xl, pl := x.fn, p.fn, x.lower, p.lower
	if p.konst && !x.konst {
		c := p.v
		if c == "" {
			return boolConst(false)
		}
		if x.slot >= 0 && !xl {
			s := x.slot
			return boolC{fn: func(r schema.Row) bool { return strings.HasPrefix(r.Str[s], c) }}
		}
		return boolC{fn: func(r schema.Row) bool {
			v := xf(r)
			return v != "" && foldHasPrefix(v, xl, c, false)
		}}
	}
	fn := func(r schema.Row) bool {
		a, b := xf(r), pf(r)
		return a != "" && b != "" && foldHasPrefix(a, xl, b, pl)
	}
	return foldBool(fn, x.konst && p.konst)
}
