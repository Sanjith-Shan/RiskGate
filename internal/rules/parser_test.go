package rules

import (
	"errors"
	"strings"
	"testing"
)

// sexpr prints e fully parenthesized, so tests can state a parse exactly.
func sexpr(e Expr) string {
	switch e := e.(type) {
	case *Unary:
		if e.Op == OpNot {
			return "(not " + sexpr(e.X) + ")"
		}
		return "(-" + sexpr(e.X) + ")"
	case *Binary:
		return "(" + sexpr(e.X) + " " + e.Op.String() + " " + sexpr(e.Y) + ")"
	case *In:
		return "(" + sexpr(e.X) + " in " + Print(e.List) + ")"
	case *Call:
		args := make([]string, len(e.Args))
		for i, a := range e.Args {
			args[i] = sexpr(a)
		}
		return e.Name + "(" + strings.Join(args, ", ") + ")"
	}
	return Print(e)
}

func mustParseExpr(t *testing.T, src string) Expr {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", src, err)
	}
	return e
}

func TestPrecedence(t *testing.T) {
	tests := []struct{ src, want string }{
		// and binds tighter than or, exactly as * binds tighter than +.
		{"true or false and true", "(true or (false and true))"},
		{"true and false or true", "((true and false) or true)"},
		{"1 + 2 * 3 > 4", "((1 + (2 * 3)) > 4)"},
		{"1 - 2 - 3 = 0", "(((1 - 2) - 3) = 0)"},
		{"8 / 4 / 2 = 1", "(((8 / 4) / 2) = 1)"},
		{"-1 * 2 < 0", "(((-1) * 2) < 0)"},
		{"- -1 < 0", "((-(-1)) < 0)"},
		{"-(1 + 2) < 0", "((-(1 + 2)) < 0)"},
		// not sits between and and the comparisons.
		{"not :amount: > 1 and true", "((not (:amount: > 1)) and true)"},
		{"not not true", "(not (not true))"},
		{"not (true or false)", "(not (true or false))"},
		{":amount: + 1 in [1, 2]", "((:amount: + 1) in [1, 2])"},
		{`:card_network: not in ["visa"] or true`, `((not (:card_network: in ["visa"])) or true)`},
		{"true or true or true", "((true or true) or true)"},
		{`is_missing(:amount:) or starts_with(lower(:device_info:), "sm")`, `(is_missing(:amount:) or starts_with(lower(:device_info:), "sm"))`},
		{"(1 < 2) = (3 < 4)", "((1 < 2) = (3 < 4))"},
	}
	for _, tt := range tests {
		if got := sexpr(mustParseExpr(t, tt.src)); got != tt.want {
			t.Errorf("%s\n got  %s\n want %s", tt.src, got, tt.want)
		}
	}
}

func TestParseRuleSet(t *testing.T) {
	src := "# header comment\n" +
		"\n" +
		"allow if :purchaser_email_domain: in @trusted_domains\r\n" +
		"   block if :risk_score: >= 85   # trailing comment with \"quotes\"\n" +
		"shadow review if :amount: > 1000\n" +
		"review if :product_code: = \"C\" and is_missing(:device_info:)"
	rules, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		shadow bool
		action Action
		text   string
		line   int
		col    int
	}{
		{false, Allow, "allow if :purchaser_email_domain: in @trusted_domains", 3, 1},
		{false, Block, "block if :risk_score: >= 85", 4, 4},
		{true, Review, "shadow review if :amount: > 1000", 5, 1},
		{false, Review, `review if :product_code: = "C" and is_missing(:device_info:)`, 6, 1},
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d", len(rules), len(want))
	}
	for i, w := range want {
		r := rules[i]
		if r.Shadow != w.shadow || r.Action != w.action || r.Text != w.text || r.Index != i ||
			r.Sp.Start.Line != w.line || r.Sp.Start.Col != w.col {
			t.Errorf("rule %d = {%v %v %q idx %d at %d:%d}, want %+v", i, r.Shadow, r.Action, r.Text, r.Index, r.Sp.Start.Line, r.Sp.Start.Col, w)
		}
	}
	if got := src[rules[1].Sp.Start.Offset:rules[1].Sp.End.Offset]; got != rules[1].Text {
		t.Errorf("rule span covers %q, want %q", got, rules[1].Text)
	}
}

func TestSpans(t *testing.T) {
	src := `:amount: > 3 * :card_mean_amount_7d: and lower(:card_type:) = "credit"`
	e := mustParseExpr(t, src)
	text := func(e Expr) string { return src[e.Span().Start.Offset:e.Span().End.Offset] }
	and := e.(*Binary)
	if got := text(and); got != src {
		t.Errorf("and span = %q", got)
	}
	cmp := and.X.(*Binary)
	if got := text(cmp.Y); got != "3 * :card_mean_amount_7d:" {
		t.Errorf("product span = %q", got)
	}
	call := and.Y.(*Binary).X.(*Call)
	if got := text(call); got != "lower(:card_type:)" {
		t.Errorf("call span = %q", got)
	}
	// Every node's span must lie inside its parent's.
	Inspect(e, func(n Expr) bool {
		sp := n.Span()
		if sp.Start.Offset < 0 || sp.End.Offset > len(src) || sp.Start.Offset > sp.End.Offset {
			t.Errorf("bad span %+v for %s", sp, Print(n))
		}
		return true
	})
}

func TestStringEscapes(t *testing.T) {
	e := mustParseExpr(t, `:device_info: = "a\"b\\c\n\t\r\u00e9é"`)
	got := e.(*Binary).Y.(*StringLit).Value
	if want := "a\"b\\c\n\t\ré\u00e9"; got != want {
		t.Errorf("decoded %q, want %q", got, want)
	}
}

func TestParseRuleErrors(t *testing.T) {
	// Messages are pinned by the golden files; this checks the plumbing:
	// every bad line is reported, and good lines still parse.
	rules, err := Parse("block if :amount: > 1\nblok if true\nreview if (\nallow if true")
	var ds Diagnostics
	if !errors.As(err, &ds) {
		t.Fatalf("err = %v, want Diagnostics", err)
	}
	if len(ds) != 2 || ds[0].Span.Start.Line != 2 || ds[1].Span.Start.Line != 3 {
		t.Fatalf("diagnostics = %v", ds)
	}
	if len(rules) != 2 || rules[0].Index != 0 || rules[1].Index != 3 {
		t.Fatalf("rules = %v", rules)
	}
	if _, err := ParseRule("block if true\nallow if true"); err == nil {
		t.Error("ParseRule accepted two rules")
	}
	if _, err := ParseExpr("true\nfalse"); err == nil {
		t.Error("ParseExpr accepted two lines")
	}
}

func TestParseErrorMessages(t *testing.T) {
	tests := []struct{ src, want string }{
		{"block if", "expected a condition after if"},
		{"shadow if true", "this rule has no action"},
		{"shadow :a: > 1", "shadow must be followed by allow, block or review"},
		{"block", "expected if after block."},
		{"block when true", "expected if after block, but found when"},
		{"block if :amount: > 1 not true", "Did you mean and not?"},
		{"block if :amount: > 1)", "has no matching ("},
		{"block if true block if true", "Put each rule on its own line"},
		{"block if @x", "a list can only come after in"},
		{"block if [1]", "can only come after in"},
		{"block if ()", "expected a value before )"},
		{"block if :amount: in (1, 2)", "Lists use square brackets"},
		{"block if :amount: in", "expected a list after in."},
		{"block if :amount: in []", "this list is empty"},
		{"block if :amount: in [1, ]", "expected a list item before ]"},
		{"block if :amount: in [1 2]", "Separate list items with commas"},
		{"block if :amount: in [1, 2", "this [ is never closed"},
		{"block if :amount: in [1, :amount:]", "cannot be a list item"},
		{"block if :card_type: in [debit]", `double quotes: "debit"`},
		{"block if lower(:card_type: :a:) = 1", "expected , or ) in the call to lower"},
		{"block if lower(:card_type: = 1", "the call to lower is never closed"},
		{"block if lower(", "the call to lower is never closed"},
		{"block if lower(:card_type:", "the call to lower is never closed"},
		{"block if + 1", "Remove the +"},
		{"block if * 1", "expected a value before *"},
		{"block if not", "the rule ends right after not"},
		{"block if :a: > 1 and", "the rule ends right after and"},
		{"block if (:a: > 1 :b:", "expected ), but found :b:"},
		{"block if .5 > 1", "Write 0.5"},
		{"block if 5. > 1", "cannot end with a decimal point"},
		{"block if 12ab > 1", "12ab is not a number"},
		{"block if 99999999" + strings.Repeat("9", 400) + " > 1", "too large"},
		{`block if :a: = "\q"`, "unknown escape"},
		{`block if :a: = "\u12"`, "unknown escape"},
		{"block if : > 1", "no attribute name"},
		{"block if @ in true", "must be followed by a list name"},
		{"block if :a: > 1 && true", "Write and instead"},
		{"block if :a: > 1 || true", "Write or instead"},
		{"block if ! true", "Write not instead"},
		{"block if :a: <> 1", "Write != instead"},
		{"block if :a: =< 1", "Write <= instead"},
		{"block if :a: => 1", "Write >= instead"},
		{"block if :a: = “x”", "curly quote"},
		{"block if :a: = 'x", "double quotes"},
		{"block if :a: $ 1", "unexpected character '$'"},
		{"block if \xff", "not valid UTF-8"},
		{"block if Not true", "Keywords are lowercase"},
		{"block if :a: > 1 AND true", "Keywords are lowercase"},
		{"block if thing", "If thing is an attribute, write :thing:"},
		{"block if :a: = true false", "expected and, or, or the end of the rule"},
		{"123 if true", "must start with allow, block or review"},
		{"approve if true", "Did you mean allow?"},
		{"flag if true", "Did you mean review?"},
		{"frobnicate if true", "A rule starts with allow, block or review"},
		{"block if true, false", "expected and, or, or the end of the rule, but found ,"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.src)
		if err == nil {
			t.Errorf("%q: no error", tt.src)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q:\n got  %v\n want it to contain %q", tt.src, err, tt.want)
		}
	}
}

func TestDiagnosticsAPI(t *testing.T) {
	var none Diagnostics
	if none.Err() != nil || none.Error() != "no errors" {
		t.Error("empty Diagnostics should not be an error")
	}
	w := Diagnostics{{Severity: SeverityWarning, Msg: "w."}}
	if w.Err() != nil || len(w.Warnings()) != 1 || w.HasErrors() {
		t.Error("warnings alone must not be an error")
	}
	if SeverityError.String() != "error" || SeverityWarning.String() != "warning" {
		t.Error("severity names")
	}
}

// TestLexErrorsBounded is the regression test for a fuzzer find: a line of
// junk produced one diagnostic per byte, each quoting the whole line, so
// rendering was quadratic in the line's length.
func TestLexErrorsBounded(t *testing.T) {
	for _, src := range []string{
		"block if :amount: > 1 " + strings.Repeat("\x7f", 5000),
		strings.Repeat("'", 5000),
		strings.Repeat(":", 5000),
	} {
		_, err := Parse(src)
		var ds Diagnostics
		if !errors.As(err, &ds) {
			t.Fatalf("err = %v", err)
		}
		if len(ds) > maxLexErrors {
			t.Errorf("%d diagnostics for one line, want at most %d", len(ds), maxLexErrors)
		}
	}
	_, err := Parse("block if :a: $%^ 1")
	if err == nil || !strings.Contains(err.Error(), "unexpected characters.") || strings.Count(err.Error(), "\n") != 0 {
		t.Errorf("a run of bad characters should be one diagnostic, got %v", err)
	}
}
