package rules

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// refEval is a deliberately naive tree-walking interpreter of the documented
// semantics. It shares no code with the compiler (no folding, no
// specialization, no byte iterators), which is what makes it an oracle.
type refVal struct {
	num float64
	str string
	b   bool
}

func refEval(e Expr, r schema.Row) refVal {
	switch e := e.(type) {
	case *Attr:
		if e.Field.Kind == schema.Number {
			return refVal{num: r.Num[e.Field.Slot]}
		}
		return refVal{str: r.Str[e.Field.Slot]}
	case *NumberLit:
		return refVal{num: e.Value}
	case *StringLit:
		return refVal{str: e.Value}
	case *BoolLit:
		return refVal{b: e.Value}
	case *Unary:
		x := refEval(e.X, r)
		if e.Op == OpNot {
			return refVal{b: !x.b}
		}
		return refVal{num: -x.num}
	case *Binary:
		x, y := refEval(e.X, r), refEval(e.Y, r)
		switch e.Op {
		case OpAnd:
			return refVal{b: x.b && y.b}
		case OpOr:
			return refVal{b: x.b || y.b}
		case OpAdd:
			return refVal{num: x.num + y.num}
		case OpSub:
			return refVal{num: x.num - y.num}
		case OpMul:
			return refVal{num: x.num * y.num}
		case OpDiv:
			if y.num == 0 {
				return refVal{num: math.NaN()}
			}
			return refVal{num: x.num / y.num}
		}
		if e.X.Type() == TypeString {
			if x.str == "" || y.str == "" {
				return refVal{}
			}
			return refVal{b: (x.str == y.str) == (e.Op == OpEq)}
		}
		if math.IsNaN(x.num) || math.IsNaN(y.num) {
			return refVal{}
		}
		var b bool
		switch e.Op {
		case OpEq:
			b = x.num == y.num
		case OpNe:
			b = x.num != y.num
		case OpLt:
			b = x.num < y.num
		case OpLe:
			b = x.num <= y.num
		case OpGt:
			b = x.num > y.num
		case OpGe:
			b = x.num >= y.num
		}
		return refVal{b: b}
	case *In:
		x := refEval(e.X, r)
		if e.Kind == schema.Number {
			for _, v := range e.Numbers {
				if !math.IsNaN(x.num) && x.num == v {
					return refVal{b: true}
				}
			}
			return refVal{}
		}
		for _, v := range e.Strings {
			if x.str != "" && x.str == v {
				return refVal{b: true}
			}
		}
		return refVal{}
	case *Call:
		switch e.Fn {
		case FuncLower:
			return refVal{str: strings.ToLower(refEval(e.Args[0], r).str)}
		case FuncIsMissing:
			x := refEval(e.Args[0], r)
			if e.Args[0].Type() == TypeNumber {
				return refVal{b: math.IsNaN(x.num)}
			}
			return refVal{b: x.str == ""}
		case FuncStartsWith:
			s, p := refEval(e.Args[0], r).str, refEval(e.Args[1], r).str
			return refVal{b: s != "" && p != "" && strings.HasPrefix(s, p)}
		}
	}
	panic("refEval: unexpected node " + Print(e))
}

func compileExpr(t testing.TB, src string) (Expr, func(schema.Row) bool) {
	t.Helper()
	r, err := ParseRule("block if " + src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if err := Check([]*Rule{r}, testEnv()); err != nil {
		t.Fatalf("check %q: %v", src, err)
	}
	fn, err := CompileExpr(r.Cond)
	if err != nil {
		t.Fatal(err)
	}
	return r.Cond, fn
}

// row builds a row from name/value pairs; unnamed fields are missing.
func row(kv ...any) schema.Row {
	cat := testEnv().Catalog
	r := cat.NewRow()
	for i := 0; i < len(kv); i += 2 {
		f := cat.MustLookup(kv[i].(string))
		switch v := kv[i+1].(type) {
		case float64:
			r.Num[f.Slot] = v
		case int:
			r.Num[f.Slot] = float64(v)
		case string:
			r.Str[f.Slot] = v
		}
	}
	return r
}

// TestMissingSemantics is the truth table in the package documentation.
func TestMissingSemantics(t *testing.T) {
	rows := []struct {
		name string
		row  schema.Row
		want [4]bool // > 100, not > 100, <= 100, is_missing
	}{
		{"150", row("amount", 150), [4]bool{true, false, false, false}},
		{"50", row("amount", 50), [4]bool{false, true, true, false}},
		{"missing", row(), [4]bool{false, true, false, true}},
	}
	exprs := []string{":amount: > 100", "not :amount: > 100", ":amount: <= 100", "is_missing(:amount:)"}
	for _, rr := range rows {
		for i, src := range exprs {
			if _, fn := compileExpr(t, src); fn(rr.row) != rr.want[i] {
				t.Errorf("amount=%s: %s = %v, want %v", rr.name, src, !rr.want[i], rr.want[i])
			}
		}
	}
}

func TestEvaluate(t *testing.T) {
	full := row("amount", 250, "card_mean_amount_7d", 50, "risk_score", 90, "billing_region", 204,
		"card_type", "Credit", "card_network", "visa", "device_info", "SM-G892A Build/NRD90M",
		"purchaser_email_domain", "GMAIL.COM", "card_txn_count_1h", 0)
	empty := row()
	tests := []struct {
		src         string
		full, empty bool
	}{
		{":amount: > 3 * :card_mean_amount_7d:", true, false},
		{":amount: / :card_txn_count_1h: > 0", false, false}, // division by zero is missing
		{"is_missing(:amount: / :card_txn_count_1h:)", true, true},
		{"is_missing(:amount: / 0)", true, true},
		{"is_missing(:amount: + 1)", false, true},
		{":amount: != 1", true, false}, // != with missing is false
		{"not :amount: = 1", true, true},
		{"-:amount: < 0", true, false},
		{`:card_type: = "credit"`, false, false},
		{`lower(:card_type:) = "credit"`, true, false},
		{`lower(:card_type:) != "credit"`, false, false},
		{`:card_type: != "credit"`, true, false},
		{`lower(:purchaser_email_domain:) in @trusted_domains`, true, false},
		{`:purchaser_email_domain: in @trusted_domains`, false, false},
		{`:purchaser_email_domain: not in @trusted_domains`, true, true},
		{`lower(:purchaser_email_domain:) = lower("Gmail.Com")`, true, false},
		{`lower(:purchaser_email_domain:) = lower(:purchaser_email_domain:)`, true, false},
		{`:card_type: = :card_type:`, true, false},
		{`starts_with(:device_info:, "SM-")`, true, false},
		{`starts_with(lower(:device_info:), "sm-")`, true, false},
		{`starts_with(:device_info:, "sm-")`, false, false},
		{`starts_with(:device_info:, :card_network:)`, false, false},
		{`starts_with(:device_info:, :device_info:)`, true, false},
		{`:billing_region: in @blocked_regions`, true, false},
		{`:billing_region: in [204, -1]`, true, false},
		{`:billing_region: + 0 in [204]`, true, false},
		{`is_missing(:device_type:)`, true, true},
		{`is_missing(lower(:card_type:))`, false, true},
		{`is_missing("x")`, false, false},
		{`:risk_score: >= 85 or :amount: > 10000`, true, false},
		{`:risk_score: >= 85 and :amount: > 10000`, false, false},
		{`true and :risk_score: > 1`, true, false},
		{`false or :risk_score: > 1`, true, false},
		{`:risk_score: > 1 and false`, false, false},
		{`:risk_score: > 1 or true`, true, true},
		{`1 / 0 = 1 / 0`, false, false},
		{`1 / 0 != 1`, false, false},
		{`5 > :risk_score:`, false, false},
		{`100 > :risk_score:`, true, false},
		{`:risk_score: = :risk_score:`, true, false},
		{`:risk_score: != :amount:`, true, false},
	}
	for _, tt := range tests {
		e, fn := compileExpr(t, tt.src)
		if got := fn(full); got != tt.full {
			t.Errorf("%s on full row = %v, want %v", tt.src, got, tt.full)
		}
		if got := fn(empty); got != tt.empty {
			t.Errorf("%s on empty row = %v, want %v", tt.src, got, tt.empty)
		}
		// The oracle must agree with the table too, or the table is wrong.
		if got := refEval(e, full).b; got != tt.full {
			t.Errorf("reference disagrees on %s: %v", tt.src, got)
		}
	}
}

// TestCompiledMatchesReference is the in-package differential test: the
// compiled closures, with all their folding and specialization, must agree
// with the naive interpreter on generated rules and rows.
func TestCompiledMatchesReference(t *testing.T) {
	env := testEnv()
	rng := rand.New(rand.NewPCG(3, 4))
	rows := make([]schema.Row, 200)
	for i := range rows {
		rows[i] = GenerateRow(rng, env.Catalog)
	}
	matches := 0
	const nRules = 3000
	for i := range nRules {
		r := Generate(rng, env, i%6)
		cr, err := Compile(r)
		if err != nil {
			t.Fatal(err)
		}
		for j, row := range rows {
			got, want := cr.Match(row), refEval(r.Cond, row).b
			if got != want {
				t.Fatalf("rule %s\nrow %d: compiled %v, reference %v\nnum %v\nstr %q", r, j, got, want, row.Num, row.Str)
			}
			if got {
				matches++
			}
		}
	}
	// Guard against a generator that only makes rules that never match.
	if frac := float64(matches) / float64(nRules*len(rows)); frac < 0.1 || frac > 0.9 {
		t.Errorf("generated rules match %.2f of rows; the generator is too lopsided to be a good test", frac)
	}
}

func TestConstantFolding(t *testing.T) {
	for _, tt := range []struct {
		src   string
		konst bool
	}{
		{"1 + 2 * 3 = 7", true},
		{`lower("ABC") = "abc"`, true},
		{`:amount: > 1 / 0`, true}, // compared with a constant missing: always false
		{`:amount: / 0 > 1`, true},
		{`false and :amount: > 1`, true},
		{`:amount: > 1 or true`, true},
		{`not (1 < 2)`, true},
		{`"a" in ["a", "b"]`, true},
		{`1 in @blocked_regions`, true},
		{`is_missing(1 / 0)`, true},
		{`starts_with("abc", "ab")`, true},
		{`true and :amount: > 1`, false},
		{`:amount: + 1 > 2`, false},
	} {
		e, _ := compileExpr(t, tt.src)
		if b := compileBool(e); b.konst != tt.konst {
			t.Errorf("%s: folded = %v, want %v", tt.src, b.konst, tt.konst)
		}
	}
}

func TestCompileRejectsUnchecked(t *testing.T) {
	r, err := ParseRule("block if :amount: > 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(r); err == nil || !strings.Contains(err.Error(), "was it checked?") {
		t.Errorf("Compile of an unchecked rule: err = %v", err)
	}
	if _, err := CompileExpr(&Binary{Op: OpAdd, X: &NumberLit{Value: 1}, Y: &NumberLit{Value: 2}}); err == nil {
		t.Error("CompileExpr accepted a number as a condition")
	}
}

func TestLongLoweredValueInLargeList(t *testing.T) {
	// A lowered value longer than the stack buffer falls back to a scan.
	long := strings.Repeat("X", lowerBufSize+10)
	env := testEnv()
	env.Lists["long"] = StringList(append([]string{strings.ToLower(long)}, strings.Split("a b c d e f g h i j", " ")...)...)
	r, err := ParseRule(`block if lower(:device_info:) in @long`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check([]*Rule{r}, env); err != nil {
		t.Fatal(err)
	}
	cr, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	if !cr.Match(row("device_info", long)) || cr.Match(row("device_info", long+"y")) || !cr.Match(row("device_info", "A")) {
		t.Error("lowered membership in a large list is wrong")
	}
}

// TestToLowerIdempotent justifies compiling lower(lower(x)) as lower(x).
func TestToLowerIdempotent(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if l := unicode.ToLower(r); unicode.ToLower(l) != l {
			t.Fatalf("unicode.ToLower is not idempotent at %U", r)
		}
	}
}

func TestFoldHelpers(t *testing.T) {
	cases := []string{"", "a", "A", "Straße", "ÀÉÎ", "\xff\xfe", "İstanbul", "ǅ", "K", "abc\x80", "SM-G892A"}
	for _, s := range cases {
		for _, u := range cases {
			checkFold(t, s, u)
		}
	}
	var buf [4]byte
	if _, ok := lowerInto(buf[:], "ABCDE"); ok {
		t.Error("lowerInto should report overflow")
	}
}

// checkFold compares the byte-iterator helpers with strings.ToLower.
func checkFold(t *testing.T, s, u string) {
	t.Helper()
	ls, lu := strings.ToLower(s), strings.ToLower(u)
	for _, c := range []struct {
		got, want bool
		what      string
	}{
		{foldEqual(s, true, u, false), ls == u, "lower(s) == u"},
		{foldEqual(s, false, u, true), s == lu, "s == lower(u)"},
		{foldEqual(s, true, u, true), ls == lu, "lower(s) == lower(u)"},
		{foldEqual(s, false, u, false), s == u, "s == u"},
		{foldHasPrefix(s, true, u, false), strings.HasPrefix(ls, u), "HasPrefix(lower(s), u)"},
		{foldHasPrefix(s, false, u, true), strings.HasPrefix(s, lu), "HasPrefix(s, lower(u))"},
		{foldHasPrefix(s, true, u, true), strings.HasPrefix(ls, lu), "HasPrefix(lower(s), lower(u))"},
		{foldHasPrefix(s, false, u, false), strings.HasPrefix(s, u), "HasPrefix(s, u)"},
	} {
		if c.got != c.want {
			t.Errorf("%s with s=%q u=%q: got %v, want %v", c.what, s, u, c.got, c.want)
		}
	}
	var buf [64]byte
	if n, ok := lowerInto(buf[:], s); ok && string(buf[:n]) != ls {
		t.Errorf("lowerInto(%q) = %q, want %q", s, buf[:n], ls)
	}
}
