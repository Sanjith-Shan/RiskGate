package rules

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func mustCheck(t *testing.T, src string) *Rule {
	t.Helper()
	r, err := ParseRule(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if err := Check([]*Rule{r}, testEnv()); err != nil {
		t.Fatalf("check %q: %v", src, err)
	}
	return r
}

func TestCheckAccepts(t *testing.T) {
	for _, src := range []string{
		"allow if :purchaser_email_domain: in @trusted_domains",
		"block if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:",
		"block if :risk_score: >= 85",
		"review if :distinct_cards_per_device_24h: > 3",
		`review if :product_code: = "C" and is_missing(:device_info:)`,
		`review if lower(:card_network:) not in ["visa", "mastercard"]`,
		`block if starts_with(lower(:device_info:), "sm-") and :billing_region: in @blocked_regions`,
		`block if :amount: / :card_mean_amount_7d: > -2.5 or not (:risk_score: < 10)`,
		`block if lower("ABC") = lower(:card_type:) and is_missing(:amount: + 1) and is_missing("x")`,
		"block if true",
		`block if :amount: in [1, -2, 3.5]`,
	} {
		mustCheck(t, src)
	}
}

func TestCheckResolves(t *testing.T) {
	r := mustCheck(t, `block if :risk_score: > 1 and lower(:card_type:) in ["b", "a", "b"] and :billing_region: in @blocked_regions`)
	cat := schema.Default()
	var attrs []*Attr
	var ins []*In
	var calls []*Call
	Inspect(r.Cond, func(e Expr) bool {
		switch e := e.(type) {
		case *Attr:
			attrs = append(attrs, e)
		case *In:
			ins = append(ins, e)
		case *Call:
			calls = append(calls, e)
		}
		return true
	})
	for _, a := range attrs {
		if want := cat.MustLookup(a.Name); a.Field != want {
			t.Errorf(":%s: resolved to %+v, want %+v", a.Name, a.Field, want)
		}
	}
	if len(calls) != 1 || calls[0].Fn != FuncLower || calls[0].Type() != TypeString {
		t.Errorf("calls = %+v", calls)
	}
	if ins[0].Kind != schema.String || !slices.Equal(ins[0].Strings, []string{"a", "b"}) {
		t.Errorf("literal list resolved to %v %v", ins[0].Kind, ins[0].Strings)
	}
	if ins[1].Kind != schema.Number || !slices.Equal(ins[1].Numbers, SampleLists()["blocked_regions"].Numbers) {
		t.Errorf("named list resolved to %v %v", ins[1].Kind, ins[1].Numbers)
	}
	if r.Cond.Type() != TypeBool {
		t.Errorf("condition type %v", r.Cond.Type())
	}
}

func TestCheckReportsEveryError(t *testing.T) {
	r, err := ParseRule(`block if :amont: > 1 and :card_type: < "x" and :nope: in @nolist and frob(1) and lower(1, 2) = "a"`)
	if err != nil {
		t.Fatal(err)
	}
	err = Check([]*Rule{r}, testEnv())
	var ds Diagnostics
	if !errors.As(err, &ds) {
		t.Fatalf("err = %v", err)
	}
	want := []string{
		"unknown attribute :amont:. Did you mean :amount:?",
		"< only compares numbers",
		"unknown attribute :nope:",
		"unknown list @nolist",
		"unknown function frob",
		"lower takes 1 argument",
	}
	if len(ds) != len(want) {
		t.Fatalf("got %d diagnostics, want %d:\n%s", len(ds), len(want), ds.Render())
	}
	for i, w := range want {
		if !strings.Contains(ds[i].Message(), w) {
			t.Errorf("diagnostic %d = %q, want it to contain %q", i, ds[i].Message(), w)
		}
	}
}

func TestCheckMessages(t *testing.T) {
	tests := []struct{ src, want string }{
		{`block if not :amount:`, "not needs a condition, but :amount: is a number"},
		{`block if -:card_type: > 1`, "- needs a number, but :card_type: is a string"},
		{`block if :card_type:`, `for example :card_type: = "some text"`},
		{`block if :amount: > 1 or "x"`, "or joins two conditions"},
		{`block if (:amount: > 1) in [1]`, "in needs a value on its left"},
		{`block if :amount: in ["1", "2"]`, "Remove the quotes from the list items"},
		{`block if :amount: in ["1", ""]`, `"" is empty text`},
		{`block if is_missing("")`, `"" is empty text`},
		{`block if is_missing(:amount: > 1)`, "is_missing needs a value"},
		{`block if starts_with(:card_type:, 1)`, "starts_with works on text, but 1 is a number"},
		{`block if :amount: = :card_type:`, ":amount: is a number, and :card_type: is a string, so they cannot be compared"},
		{`block if -5 = :card_type:`, `Put it in quotes: "-5"`},
		{`block if :amount: in @nolist`, "Known lists: @blocked_regions"},
		{`block if :amount: in @blocked_region`, "Did you mean @blocked_regions?"},
		{`block if :card_type: in @blocked_regions`, ":card_type: is a string, but @blocked_regions holds numbers"},
		{`block if :Amount: > 1`, "Did you mean :amount:?"},
		{`block if :email_domain: = "x"`, "unknown attribute :email_domain:"},
		{`block if :risk: > 1`, "Did you mean :risk_score:?"},
		{`block if :xyzzy: > 1`, "unknown attribute :xyzzy:."},
	}
	for _, tt := range tests {
		r, err := ParseRule(tt.src)
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		err = Check([]*Rule{r}, testEnv())
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s:\n got  %v\n want it to contain %q", tt.src, err, tt.want)
		}
	}
}

func TestSuggest(t *testing.T) {
	names := schema.Default().Names()
	tests := []struct{ in, want string }{
		{"card_txn_cnt_1h", "card_txn_count_1h"},
		{"card_txn_cuont_1h", "card_txn_count_1h"}, // transposition is one edit
		{"AMOUNT", "amount"},
		{"riskscore", "risk_score"},
		{"device", ""}, // too short and ambiguous to guess
		{"zzzzzzzz", ""},
	}
	for _, tt := range tests {
		if got := suggest(tt.in, names); got != tt.want {
			t.Errorf("suggest(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if d := editDistance("ab", "ba"); d != 1 {
		t.Errorf("transposition distance = %d, want 1", d)
	}
	if d := editDistance("", "abc"); d != 3 {
		t.Errorf("distance to empty = %d, want 3", d)
	}
}

func TestListConstructors(t *testing.T) {
	s := StringList("b", "", "a", "b")
	if !slices.Equal(s.Strings, []string{"a", "b"}) || s.Kind != schema.String {
		t.Errorf("StringList = %+v", s)
	}
	n := NumberList(2, schema.Missing, 1, 2, 0)
	if !slices.Equal(n.Numbers, []float64{0, 1, 2}) || n.Kind != schema.Number {
		t.Errorf("NumberList = %+v", n)
	}
}

// TestNestedErrorsStayBounded is the regression test for a fuzzer find:
// every level of is_missing(is_missing(...)) reported an error quoting its
// whole subtree, so checking was quadratic in the nesting depth.
func TestNestedErrorsStayBounded(t *testing.T) {
	const depth = 5000
	src := "block if " + strings.Repeat("is_missing(", depth) + ":amount:" + strings.Repeat(")", depth)
	_, err := Load(src, testEnv(), 1)
	var ds Diagnostics
	if !errors.As(err, &ds) {
		t.Fatalf("err = %v", err)
	}
	if len(ds) != 1 {
		t.Errorf("got %d diagnostics, want 1", len(ds))
	}
	if n := len(ds[0].Message()); n > 200 {
		t.Errorf("message is %d bytes; it should not quote the nested expression", n)
	}
}

func TestNames(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{TypeNumber.String(), "number"}, {TypeString.String(), "string"},
		{TypeBool.String(), "true/false condition"}, {TypeInvalid.String(), "invalid"},
		{OpNe.String(), "!="}, {OpNeg.String(), "-"}, {Op(200).String(), "Op(200)"},
		{FuncStartsWith.String(), "starts_with"}, {Func(9).String(), "Func(9)"},
		{Review.String(), "review"}, {Action(9).String(), "Action(9)"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	for _, e := range []Expr{&ListRef{}, &ListLit{}} {
		if e.Type() != TypeInvalid {
			t.Errorf("%T has a type", e)
		}
	}
}
