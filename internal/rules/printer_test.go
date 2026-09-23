package rules

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestPrintCanonical(t *testing.T) {
	tests := []struct{ src, want string }{
		{"(:amount:>100)", ":amount: > 100"},
		{"((a_b_c() ))", "a_b_c()"},
		{"true or (false and true)", "true or false and true"},
		{"(true or false) and true", "(true or false) and true"},
		{"true and (false and true)", "true and (false and true)"},
		{"(true and false) and true", "true and false and true"},
		{"1 - (2 - 3) = 0", "1 - (2 - 3) = 0"},
		{"(1 - 2) - 3 = 0", "1 - 2 - 3 = 0"},
		{"(1 + 2) * 3 = 9", "(1 + 2) * 3 = 9"},
		{"-(1 * 2) = -2", "-(1 * 2) = -2"},
		{"(-1) * 2 = -2", "-1 * 2 = -2"},
		{"-(-1) = 1", "--1 = 1"},
		{"not (:amount: > 1)", "not :amount: > 1"},
		{"not (true and false)", "not (true and false)"},
		{"(not true) and false", "not true and false"},
		{"(1 < 2) = (3 < 4)", "(1 < 2) = (3 < 4)"},
		{"(not true) = false", "(not true) = false"},
		{"true = not false", "true = (not false)"},
		{":card_type: not in [\"b\", \"a\"]", `not :card_type: in ["b", "a"]`},
		{"(:amount: < 1) in [1]", "(:amount: < 1) in [1]"},
		{":amount: in [ -1 ,2.50, 0.1 ]", ":amount: in [-1, 2.5, 0.1]"},
		{"is_missing( :amount: )", "is_missing(:amount:)"},
		{"starts_with(:a:,\"x\")", `starts_with(:a:, "x")`},
		{`:a: = "q\"\\\n\t\r\u0001é"`, `:a: = "q\"\\\n\t\r\u0001é"`},
		{"0.1 + 0.2 > 000.30", "0.1 + 0.2 > 0.3"},
		{"123456789012345678901234567890 > 1", "123456789012345680000000000000 > 1"},
	}
	for _, tt := range tests {
		e := mustParseExpr(t, tt.src)
		got := Print(e)
		if got != tt.want {
			t.Errorf("Print(parse(%q)) = %q, want %q", tt.src, got, tt.want)
		}
		again := mustParseExpr(t, got)
		if !Equal(e, again) {
			t.Errorf("parse(print(%q)) = %s, want %s", tt.src, sexpr(again), sexpr(e))
		}
	}
}

func TestRuleString(t *testing.T) {
	r, err := ParseRule("  shadow   block if(:amount: > 1)  # note")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.String(), "shadow block if :amount: > 1"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestPrintRoundTripGenerated is the property parse(print(ast)) == ast over
// generated trees, which cover shapes a human would never type.
func TestPrintRoundTripGenerated(t *testing.T) {
	env := testEnv()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 5000 {
		e := GenerateExpr(rng, env, 1+i%6)
		assertRoundTrip(t, e)
	}
}

// assertRoundTrip checks that e prints to text that parses back to e and
// that printing is then a fixed point.
func assertRoundTrip(t *testing.T, e Expr) {
	t.Helper()
	text := Print(e)
	back, err := ParseExpr(text)
	if err != nil {
		t.Fatalf("printed text does not parse: %s\n%v", text, err)
	}
	if !Equal(e, back) {
		t.Fatalf("round trip changed the tree:\n text %s\n want %s\n got  %s", text, sexpr(e), sexpr(back))
	}
	if again := Print(back); again != text {
		t.Fatalf("printing is not a fixed point: %q then %q", text, again)
	}
}

func TestPrintHandBuiltTrees(t *testing.T) {
	// Trees the parser can never produce from a single token, like a
	// negated negative or a comparison of comparisons, must still survive.
	n := func(v float64) Expr { return &NumberLit{Value: v} }
	a := &Attr{Name: "amount"}
	trees := []Expr{
		&Binary{Op: OpEq, X: &Unary{Op: OpNeg, X: &Unary{Op: OpNeg, X: n(1)}}, Y: n(1)},
		&Binary{Op: OpLt, X: &Binary{Op: OpLt, X: a, Y: n(1)}, Y: &Binary{Op: OpGe, X: a, Y: n(2)}},
		&Unary{Op: OpNot, X: &Unary{Op: OpNot, X: &In{X: a, List: &ListRef{Name: "l"}}}},
		&Binary{Op: OpAnd, X: &Unary{Op: OpNot, X: &BoolLit{Value: true}}, Y: &Binary{Op: OpOr, X: &BoolLit{}, Y: &BoolLit{}}},
		&Binary{Op: OpSub, X: a, Y: &Unary{Op: OpNeg, X: a}},
		&Binary{Op: OpGt, X: n(math.MaxFloat64), Y: n(math.SmallestNonzeroFloat64)},
		&In{X: &Binary{Op: OpDiv, X: a, Y: n(0)}, List: &ListLit{Items: []Expr{&Unary{Op: OpNeg, X: n(0.5)}, n(3)}}},
	}
	for _, e := range trees {
		assertRoundTrip(t, e)
	}
}

func TestEqual(t *testing.T) {
	a := mustParseExpr(t, `:amount: > 1 and lower(:card_type:) in ["a"] or not is_missing(:x:)`)
	b := mustParseExpr(t, `(:amount: > 1) and (lower(:card_type:) in ["a"]) or (not is_missing(:x:))`)
	if !Equal(a, b) {
		t.Error("equal trees compare unequal")
	}
	for _, other := range []string{
		`:amount: > 2 and lower(:card_type:) in ["a"] or not is_missing(:x:)`,
		`:amount: >= 1 and lower(:card_type:) in ["a"] or not is_missing(:x:)`,
		`:amount: > 1 and lower(:card_type:) in ["b"] or not is_missing(:x:)`,
		`:amount: > 1 and lower(:card_type:) in @a or not is_missing(:x:)`,
		`:amount: > 1 and lower(:card_type:) in ["a"] or not is_missing(:y:)`,
		`:amount: > 1 and lower(:card_type:) in ["a"] or is_missing(:x:)`,
		`:amount: > 1 and lower(:card_type:) in ["a"] or not true`,
	} {
		if Equal(a, mustParseExpr(t, other)) {
			t.Errorf("different trees compare equal: %s", other)
		}
	}
	if !Equal(nil, nil) || Equal(a, nil) || Equal(nil, a) {
		t.Error("nil handling")
	}
}
