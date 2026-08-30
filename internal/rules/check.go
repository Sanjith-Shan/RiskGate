package rules

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// List is a named list of values that rules test with `in @name`. A list
// holds either strings or numbers, never both.
type List struct {
	Kind    schema.Kind
	Strings []string
	Numbers []float64
}

// StringList returns a list of strings, sorted and deduplicated. Empty
// strings are dropped: empty means missing, and missing is never in a list.
func StringList(values ...string) List {
	return List{Kind: schema.String, Strings: normStrings(values)}
}

// NumberList returns a list of numbers, sorted and deduplicated, with NaN
// (missing) dropped for the same reason.
func NumberList(values ...float64) List {
	return List{Kind: schema.Number, Numbers: normNumbers(values)}
}

func normStrings(vs []string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if v != "" {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func normNumbers(vs []float64) []float64 {
	out := make([]float64, 0, len(vs))
	for _, v := range vs {
		if !math.IsNaN(v) {
			out = append(out, v+0) // +0 turns -0 into 0, so sorting and dedup agree with ==
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Lists maps a list name, without its @, to its values.
type Lists map[string]List

// Env is what a rule set is checked against: the attributes that exist and
// the named lists that exist.
type Env struct {
	Catalog *schema.Catalog
	Lists   Lists
}

func (env Env) listNames(kind schema.Kind) []string {
	var out []string
	for name, l := range env.Lists {
		if kind == 0 || l.Kind == kind {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Check type-checks rules against env and resolves them in place: attributes
// get their Field, calls their Fn, and each In its list's values. Every rule
// is checked, and every independent error in a rule is reported, so the
// returned error (a Diagnostics) lists all of them. Check does not produce
// warnings; see Lint.
//
// The typing rules are small:
//
//	number op number        -> number     for + - * /
//	-number                 -> number
//	x cmp y                 -> condition  both numbers, or both strings with = or !=
//	x in list               -> condition  x's type matches the list's
//	c and c, c or c, not c  -> condition
//	is_missing(number|string), starts_with(string, string) -> condition
//	lower(string)           -> string
//
// and the rule's condition must be a condition.
func Check(rules []*Rule, env Env) error {
	var ds Diagnostics
	for _, r := range rules {
		ds = append(ds, checkRule(r, env)...)
	}
	return ds.Err()
}

func checkRule(r *Rule, env Env) Diagnostics {
	c := &checker{env: env, line: r.line()}
	switch t := c.expr(r.Cond); t {
	case TypeBool, TypeInvalid:
	default:
		c.errorf(r.Cond.Span(), conditionHint(r.Cond, t), "the condition must be true or false, but %s is a %s.", describe(r.Cond), t)
	}
	return c.errs
}

func conditionHint(e Expr, t Type) string {
	if t == TypeNumber {
		return fmt.Sprintf("Compare it with a value, for example %s > 100.", describe(e))
	}
	d := describe(e)
	if d == "this expression" {
		return `Compare it with a value, or test it with is_missing.`
	}
	return fmt.Sprintf(`Compare it with a value, for example %s = "some text", or test it with is_missing(%s).`, d, d)
}

// line returns the full source line the rule came from, for diagnostics.
func (r *Rule) line() string {
	if r.src != "" {
		return r.src
	}
	return r.Text
}

type checker struct {
	env  Env
	line string
	errs Diagnostics
}

func (c *checker) errorf(sp Span, hint, format string, args ...any) {
	c.errs = append(c.errs, Diagnostic{Span: sp, Line: c.line, Msg: fmt.Sprintf(format, args...), Hint: hint})
}

// describe names an expression in a message: attributes and literals as
// written, short expressions as printed, anything else generically. It
// never prints a large tree, which keeps the checker linear even when every
// level of a deeply nested expression has something to report.
func describe(e Expr) string {
	const maxNodes, maxLen = 8, 40
	nodes := 0
	Inspect(e, func(Expr) bool {
		nodes++
		return nodes <= maxNodes
	})
	if nodes <= maxNodes {
		if s := Print(e); len(s) <= maxLen {
			return s
		}
	}
	return "this expression"
}

// expr checks e and returns its type, or TypeInvalid after reporting an
// error. Parents stay quiet about TypeInvalid operands, so one mistake
// produces one message rather than a cascade.
func (c *checker) expr(e Expr) Type {
	switch e := e.(type) {
	case *Attr:
		return c.attr(e)
	case *NumberLit:
		return TypeNumber
	case *StringLit:
		if e.Value == "" {
			c.errorf(e.Sp, "To test for a missing value, use is_missing(:attribute:).", `"" is empty text, and empty means missing.`)
			return TypeInvalid
		}
		return TypeString
	case *BoolLit:
		return TypeBool
	case *Unary:
		t := c.expr(e.X)
		if e.Op == OpNot {
			if t != TypeBool && t != TypeInvalid {
				c.errorf(e.X.Span(), conditionHint(e.X, t), "not needs a condition, but %s is a %s.", describe(e.X), t)
			}
			return TypeBool
		}
		if t != TypeNumber && t != TypeInvalid {
			c.errorf(e.X.Span(), "", "- needs a number, but %s is a %s.", describe(e.X), t)
			return TypeInvalid
		}
		return t
	case *Binary:
		return c.binary(e)
	case *In:
		return c.in(e)
	case *Call:
		return c.call(e)
	}
	c.errorf(e.Span(), "", "%s cannot be used here.", describe(e))
	return TypeInvalid
}

func (c *checker) attr(e *Attr) Type {
	f, ok := c.env.Catalog.Lookup(e.Name)
	if !ok {
		hint := ""
		if s := suggest(e.Name, c.env.Catalog.Names()); s != "" {
			hint = fmt.Sprintf("Did you mean :%s:?", s)
		}
		c.errorf(e.Sp, hint, "unknown attribute :%s:.", e.Name)
		return TypeInvalid
	}
	e.Field = f
	return typeOfKind(f.Kind)
}

func (c *checker) binary(e *Binary) Type {
	if e.Op.IsComparison() {
		return c.comparison(e)
	}
	tx, ty := c.expr(e.X), c.expr(e.Y)
	want, result := TypeNumber, TypeNumber
	var what string
	switch e.Op {
	case OpAnd, OpOr:
		want, result = TypeBool, TypeBool
		what = e.Op.String() + " joins two conditions"
	default:
		what = e.Op.String() + " works on numbers"
	}
	ok := true
	for _, side := range []struct {
		x Expr
		t Type
	}{{e.X, tx}, {e.Y, ty}} {
		if side.t == TypeInvalid {
			ok = false
			continue
		}
		if side.t != want {
			ok = false
			hint := ""
			if want == TypeBool {
				hint = conditionHint(side.x, side.t)
			}
			c.errorf(side.x.Span(), hint, "%s, but %s is a %s.", what, describe(side.x), side.t)
		}
	}
	if !ok && result == TypeNumber {
		return TypeInvalid
	}
	return result
}

// numericText reports whether s is a number someone put in quotes, written
// the way the lexer would accept it without quotes.
func numericText(s string) bool {
	s = strings.TrimPrefix(strings.TrimSpace(s), "-")
	digits, frac, hasFrac := strings.Cut(s, ".")
	isDigits := func(t string) bool {
		return t != "" && strings.Trim(t, "0123456789") == ""
	}
	return isDigits(digits) && (!hasFrac || isDigits(frac))
}

// numberLit returns the number literal e is (possibly negated), if it is one.
func numberLit(e Expr) (string, bool) {
	switch e := e.(type) {
	case *NumberLit:
		return Print(e), true
	case *Unary:
		if _, ok := e.X.(*NumberLit); ok && e.Op == OpNeg {
			return Print(e), true
		}
	}
	return "", false
}

func (c *checker) comparison(e *Binary) Type {
	// "" gets a message that names the right fix for this comparison.
	for _, side := range [2]struct{ lit, other Expr }{{e.X, e.Y}, {e.Y, e.X}} {
		if s, ok := side.lit.(*StringLit); ok && s.Value == "" {
			hint := fmt.Sprintf("To test for a missing value, write is_missing(%s).", describe(side.other))
			if e.Op == OpNe {
				hint = fmt.Sprintf("To test for a present value, write not is_missing(%s).", describe(side.other))
			}
			c.errorf(s.Sp, hint, `"" is empty text, and empty means missing.`)
			c.expr(side.other)
			return TypeBool
		}
	}
	tx, ty := c.expr(e.X), c.expr(e.Y)
	if tx == TypeInvalid || ty == TypeInvalid {
		return TypeBool
	}
	for _, side := range [2]struct {
		x Expr
		t Type
	}{{e.X, tx}, {e.Y, ty}} {
		if side.t == TypeBool {
			c.errorf(side.x.Span(), "Use the condition on its own, or not for its opposite.",
				"%s compares values, but %s is already a true/false condition.", e.Op, describe(side.x))
			return TypeBool
		}
	}
	if tx != ty {
		num, str := e.X, e.Y
		if tx == TypeString {
			num, str = e.Y, e.X
		}
		if s, ok := str.(*StringLit); ok && numericText(s.Value) {
			c.errorf(s.Sp, "Remove the quotes.", "%s is a number, and %s is a string.", describe(num), Print(s))
		} else if n, ok := numberLit(num); ok {
			c.errorf(num.Span(), fmt.Sprintf("Put it in quotes: %s.", strconv.Quote(n)), "%s is a string, and %s is a number.", describe(str), n)
		} else {
			c.errorf(e.Sp, "", "%s is a number, and %s is a string, so they cannot be compared.", describe(num), describe(str))
		}
		return TypeBool
	}
	if tx == TypeString && e.Op != OpEq && e.Op != OpNe {
		c.errorf(e.Sp, "Text can be compared with = and !=, or tested with starts_with.",
			"%s only compares numbers, and %s is text.", e.Op, describe(e.X))
	}
	return TypeBool
}

func (c *checker) in(e *In) Type {
	tx := c.expr(e.X)
	if tx == TypeBool {
		c.errorf(e.X.Span(), "", "in needs a value on its left, but %s is a true/false condition.", describe(e.X))
		tx = TypeInvalid
	}
	var (
		kind schema.Kind
		strs []string
		nums []float64
		ok   bool
	)
	switch l := e.List.(type) {
	case *ListRef:
		kind, strs, nums, ok = c.listRef(l, tx)
	case *ListLit:
		kind, strs, nums, ok = c.listLit(l)
	}
	if !ok || tx == TypeInvalid {
		return TypeBool
	}
	if typeOfKind(kind) != tx {
		hint := ""
		if l, lit := e.List.(*ListLit); lit && kind == schema.String && allNumericText(l) {
			hint = "Remove the quotes from the list items."
		}
		c.errorf(e.Sp, hint, "%s is a %s, but %s holds %ss.", describe(e.X), tx, describe(e.List), typeOfKind(kind))
		return TypeBool
	}
	e.Kind, e.Strings, e.Numbers = kind, strs, nums
	return TypeBool
}

func allNumericText(l *ListLit) bool {
	for _, it := range l.Items {
		if s, ok := it.(*StringLit); !ok || !numericText(s.Value) {
			return false
		}
	}
	return true
}

func (c *checker) listRef(l *ListRef, want Type) (schema.Kind, []string, []float64, bool) {
	list, ok := c.env.Lists[l.Name]
	if !ok {
		var kind schema.Kind
		if want == TypeNumber {
			kind = schema.Number
		} else if want == TypeString {
			kind = schema.String
		}
		hint := ""
		if s := suggest(l.Name, c.env.listNames(kind)); s != "" {
			hint = fmt.Sprintf("Did you mean @%s?", s)
		} else if s := suggest(l.Name, c.env.listNames(0)); s != "" {
			hint = fmt.Sprintf("Did you mean @%s?", s)
		} else if names := c.env.listNames(0); len(names) > 0 && len(names) <= 8 {
			hint = "Known lists: @" + strings.Join(names, ", @") + "."
		}
		c.errorf(l.Sp, hint, "unknown list @%s.", l.Name)
		return 0, nil, nil, false
	}
	switch list.Kind {
	case schema.String:
		return list.Kind, normStrings(list.Strings), nil, true
	case schema.Number:
		return list.Kind, nil, normNumbers(list.Numbers), true
	}
	c.errorf(l.Sp, "", "list @%s has no element type.", l.Name)
	return 0, nil, nil, false
}

func (c *checker) listLit(l *ListLit) (schema.Kind, []string, []float64, bool) {
	var (
		kind schema.Kind
		strs []string
		nums []float64
		ok   = true
	)
	for _, it := range l.Items {
		var k schema.Kind
		switch it := it.(type) {
		case *StringLit:
			if it.Value == "" {
				c.errorf(it.Sp, "Remove it; a missing value is never in a list. Use is_missing to test for one.", `"" is empty text, and empty means missing.`)
				ok = false
				continue
			}
			k = schema.String
			strs = append(strs, it.Value)
		case *NumberLit:
			k = schema.Number
			nums = append(nums, it.Value)
		case *Unary:
			n, isNum := it.X.(*NumberLit)
			if it.Op != OpNeg || !isNum {
				c.errorf(it.Span(), "", "%s cannot be a list item.", describe(it))
				return 0, nil, nil, false
			}
			k = schema.Number
			nums = append(nums, -n.Value)
		default:
			c.errorf(it.Span(), "", "%s cannot be a list item.", describe(it))
			return 0, nil, nil, false
		}
		if kind == 0 {
			kind = k
		} else if k != kind {
			c.errorf(it.Span(), "Use all text in quotes or all plain numbers.", "a list cannot mix text and numbers.")
			return 0, nil, nil, false
		}
	}
	if kind == schema.String {
		return kind, normStrings(strs), nil, ok
	}
	return kind, nil, normNumbers(nums), ok
}

func (c *checker) call(e *Call) Type {
	b, ok := lookupFunc(e.Name)
	if !ok {
		names := make([]string, len(builtins))
		for i, b := range builtins {
			names[i] = b.name
		}
		hint := "The functions are is_missing, lower and starts_with."
		if s := suggest(e.Name, names); s != "" {
			hint = fmt.Sprintf("Did you mean %s?", s)
		}
		nameEnd := Pos{Offset: e.Sp.Start.Offset + len(e.Name), Line: e.Sp.Start.Line, Col: e.Sp.Start.Col + utf8.RuneCountInString(e.Name)}
		c.errorf(Span{e.Sp.Start, nameEnd},
			hint, "unknown function %s.", e.Name)
		for _, a := range e.Args {
			c.expr(a)
		}
		return TypeInvalid
	}
	if n := b.fn.arity(); len(e.Args) != n {
		plural := "s"
		if n == 1 {
			plural = ""
		}
		c.errorf(e.Sp, "For example: "+b.example, "%s takes %d argument%s (%s), but was given %d.", b.name, n, plural, b.params, len(e.Args))
		return TypeInvalid
	}
	e.Fn = b.fn
	ok = true
	for _, a := range e.Args {
		t := c.expr(a)
		switch {
		case t == TypeInvalid:
			ok = false
		case b.fn == FuncIsMissing && t == TypeBool:
			c.errorf(a.Span(), "Pass it an attribute, like is_missing(:device_info:).", "is_missing needs a value, but %s is a true/false condition.", describe(a))
			ok = false
		case b.fn != FuncIsMissing && t != TypeString:
			c.errorf(a.Span(), "", "%s works on text, but %s is a %s.", b.name, describe(a), t)
			ok = false
		}
	}
	if !ok {
		return TypeInvalid
	}
	return e.Type()
}
