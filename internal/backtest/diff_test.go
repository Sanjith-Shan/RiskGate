package backtest

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func testEnv() rules.Env {
	return rules.Env{Catalog: schema.Default(), Lists: rules.SampleLists()}
}

// TestDifferential is experiment 3 at test scale: thousands of generated
// rules, over tables ranging from lightly to almost entirely missing and
// salted with adversarial values, must evaluate identically in both back
// ends. cmd/backtest difftest runs the same check at full scale.
func TestDifferential(t *testing.T) {
	env := testEnv()
	rows, nrules := 2000, 2500
	if testing.Short() || raceEnabled {
		rows, nrules = 500, 400
	}
	tables := []struct {
		name string
		opt  SynthOptions
	}{
		{"generator rows", SynthOptions{Rows: rows, Seed: 1}},
		{"edge values", SynthOptions{Rows: rows, Seed: 2, Edge: 0.15}},
		{"60% missing", SynthOptions{Rows: rows, Seed: 3, Missing: 0.5, Edge: 0.05}},
		{"95% missing", SynthOptions{Rows: rows, Seed: 4, Missing: 0.94, Edge: 0.05}},
	}
	var total DiffResult
	for i, tc := range tables {
		for _, ground := range []bool{false, true} {
			name := tc.name
			if ground {
				name += "/grounded"
			}
			t.Run(name, func(t *testing.T) {
				tbl := Synthetic(env.Catalog, tc.opt)
				res, err := DiffTest(tbl, env, DiffOptions{
					Rules: nrules, Seed: uint64(100*i) + b2u64(ground), MinDepth: 0, MaxDepth: 6, Ground: ground,
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range res.Examples {
					t.Errorf("row %d: closure %v, vector %v\n  %s", d.Row, d.Closure, d.Vector, d.Rule)
				}
				if res.Disagreements != 0 {
					t.Fatalf("%d disagreements over %d rules", res.Disagreements, res.BadRules)
				}
				// The test is only as good as its rules: most must match
				// some rows and miss others.
				if res.Trivial > res.Rules*3/4 {
					t.Errorf("%d of %d rules are trivial (match no row or every row)", res.Trivial, res.Rules)
				}
				if ground && res.Grounded < res.Rules/4 {
					t.Errorf("only %d of %d rules were grounded", res.Grounded, res.Rules)
				}
				total.Rules += res.Rules
				total.Checked += res.Checked
				total.Matched += res.Matched
			})
		}
	}
	t.Logf("%d rules, %d rule-row pairs checked, %d matches, 0 disagreements", total.Rules, total.Checked, total.Matched)
}

func b2u64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// edgeRules are hand-written rules aimed at the places two implementations
// of the semantics are most likely to part ways.
var edgeRules = []string{
	// != and missing, negation over missing.
	`block if :amount: != 5`,
	`block if not :amount: = 5`,
	`block if :amount: != :distance:`,
	`block if not (:amount: < :distance:)`,
	`block if :purchaser_email_domain: != "gmail.com"`,
	`block if :purchaser_email_domain: != :recipient_email_domain:`,
	`block if not :purchaser_email_domain: = :recipient_email_domain:`,
	// Division by zero, constant and per row, and NaN propagation.
	`block if :amount: / 0 = :amount: / 0`,
	`block if is_missing(:amount: / 0)`,
	`block if is_missing(:amount: / :distance:)`,
	`block if :amount: / :distance: > 1`,
	`block if not (:amount: / (:distance: - :distance:) > 0)`,
	`block if 1 / 0 != 1 / 0`,
	`block if is_missing(1 / 0)`,
	`block if 0 / :amount: = 0`,
	`block if -0 = 0`,
	`block if :amount: = -0`,
	`block if :amount: in [0]`,
	`block if -:amount: in [0, 1]`,
	`block if :amount: * 0 = 0`,
	`block if :amount: - :amount: = 0`,
	`block if :amount: * :amount: > 1000000`,
	`block if :amount: + :distance: >= 179769313486231570000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000`,
	`block if :amount: * 10 > :distance: * 10`,
	`block if :amount: * 3 + :distance: = :amount: * 3 + :distance:`,
	`block if 0.1 + 0.2 = 0.3`,
	`block if :amount: in [0.3, 49.99, 1000000000000000.2]`,
	`block if :amount: in [100, 100.0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 0.30000000000000004]`,
	`block if :amount: + 0 in [0.30000000000000004]`,
	`block if :amount: in @blocked_regions or :billing_region: in @blocked_regions`,
	// lower() on non-ASCII and invalid UTF-8; Kelvin sign lowers to "k".
	`block if lower(:purchaser_email_domain:) = "k"`,
	`block if lower(:purchaser_email_domain:) = "i̇stanbul"`,
	`block if lower(:purchaser_email_domain:) = "gmail.com"`,
	`block if lower(:purchaser_email_domain:) = lower(:recipient_email_domain:)`,
	`block if lower(:purchaser_email_domain:) = :recipient_email_domain:`,
	`block if lower(:purchaser_email_domain:) != :purchaser_email_domain:`,
	`block if lower(:purchaser_email_domain:) in ["gmail.com", "Gmail.com", "k", "ß", "ǆ", "σασ"]`,
	`block if lower(:purchaser_email_domain:) in ["a", "b", "c", "d", "e", "f", "g", "h", "i", "gmail.com", "k"]`,
	`block if lower(:device_info:) = "gmail�"`,
	`block if lower(:device_info:) = "��"`,
	`block if starts_with(lower(:device_info:), "gmail")`,
	`block if starts_with(:device_info:, lower(:purchaser_email_domain:))`,
	`block if starts_with(lower(:purchaser_email_domain:), lower(:recipient_email_domain:))`,
	`block if starts_with("gmail.com", :purchaser_email_domain:)`,
	`block if starts_with(lower("GMAIL.COM"), lower(:purchaser_email_domain:))`,
	`block if is_missing(lower(:purchaser_email_domain:))`,
	`block if not is_missing(lower(lower(:device_info:)))`,
	`block if lower("K") = "k"`,
	`block if lower(lower(:purchaser_email_domain:)) = lower(:purchaser_email_domain:)`,
	`block if :purchaser_email_domain: = :purchaser_email_domain:`,
	`block if :purchaser_email_domain: in @trusted_domains`,
	`block if not :purchaser_email_domain: in @trusted_domains`,
	// Right-nested conjunctions (the parentheses keep them nested), so a
	// sparse left side refines a whole and-chain on its right, including
	// sides that cannot refine and fall back to full evaluation.
	`block if :amount: = 49.99 and (:distance: > 5 and lower(:purchaser_email_domain:) = "gmail.com")`,
	`block if :amount: = 49.99 and (not is_missing(:distance:) and :risk_score: >= 50)`,
	`block if :amount: = 49.99 and (:distance: * 2 > 5 and (:amount: != :distance: and :billing_region: in [123, 204, 299, 325, 441, 100, 101, 102, 103]))`,
	`block if :amount: = 49.99 and (is_missing(:risk_score:) and :purchaser_email_domain: != :recipient_email_domain:)`,
	`block if :amount: = 49.99 and (false and :distance: > 1)`,
	`block if :amount: = 49.99 and (true and :distance: > 1)`,
	`block if is_missing(:amount:) and (:amount: = 1 and :distance: > 1)`,
	`block if :amount: = 49.99 and :distance: + 1 > 5`,
	`block if :amount: = 49.99 and :amount: in [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 49.99]`,
	`block if :amount: = 49.99 and :amount: >= :distance:`,
	`block if :amount: = 49.99 and is_missing(:distance: / 2)`,
	// Number lists on each kernel: a short list (linear scan), longer ones
	// (hash table), and their negations, around zero and the edge values.
	`block if :amount: in [0, 1]`,
	`block if not :amount: in [0, 1, 2]`,
	`block if :amount: in [0, 1, 2, 3, 49.99]`,
	`block if not :distance: in [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20]`,
	`block if :distance: in [0.5, 9007199254740993, 0.000001, 2.5, 100, 1000, 10000, 123456789012345678901234567890]`,
	`block if -:amount: in [0, 1, 2, 3, 4, 5]`,
	// Constant conditions and short circuits.
	`block if true`,
	`block if false or not true`,
	`block if :amount: > 5 and false`,
	`block if :amount: > 5 or true`,
	`block if not (:amount: > 5 or :distance: > 5) and not is_missing(:amount:)`,
}

func TestDifferentialEdgeRules(t *testing.T) {
	env := testEnv()
	var rs []*rules.Rule
	for _, src := range edgeRules {
		r, err := rules.ParseRule(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if err := rules.Check([]*rules.Rule{r}, env); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		rs = append(rs, r)
	}
	for _, opt := range []SynthOptions{
		{Rows: 2000, Seed: 7, Edge: 0.3},
		{Rows: 2000, Seed: 8, Edge: 0.6, Missing: 0.3},
		{Rows: 130, Seed: 9, Edge: 0.9}, // not a multiple of 64: exercises the tail word
	} {
		tbl := Synthetic(env.Catalog, opt)
		res, err := DiffRules(tbl, tbl.Rows(), rs, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Examples {
			t.Errorf("row %d (%v): closure %v, vector %v\n  %s", d.Row, describeRow(tbl, d.Row), d.Closure, d.Vector, d.Rule)
		}
	}
}

func describeRow(t *Table, i int) string {
	var b strings.Builder
	for _, name := range []string{"amount", "distance", "purchaser_email_domain", "recipient_email_domain", "device_info"} {
		f := t.Catalog.MustLookup(name)
		if f.Kind == schema.Number {
			fmt.Fprintf(&b, "%s=%v ", name, t.Num[f.Slot][i])
		} else {
			fmt.Fprintf(&b, "%s=%q ", name, t.Dict[f.Slot][t.Str[f.Slot][i]])
		}
	}
	return b.String()
}

// TestEdgeValuesPresent guards the edge tests against a quiet regression in
// Synthetic: the adversarial values must really be in the table.
func TestEdgeValuesPresent(t *testing.T) {
	tbl := Synthetic(schema.Default(), SynthOptions{Rows: 3000, Seed: 7, Edge: 0.3})
	amount := tbl.Num[tbl.Catalog.MustLookup("amount").Slot]
	var negZero, inf bool
	for _, v := range amount {
		negZero = negZero || (v == 0 && math.Signbit(v))
		inf = inf || math.IsInf(v, 0)
	}
	if !negZero || !inf {
		t.Errorf("edge numbers missing: -0 %v, inf %v", negZero, inf)
	}
	email := tbl.Catalog.MustLookup("purchaser_email_domain").Slot
	var kelvin bool
	for _, s := range tbl.Dict[email] {
		kelvin = kelvin || s == "K"
	}
	if !kelvin {
		t.Error("Kelvin sign missing from the email dictionary")
	}
}

func TestGroundKeepsRuleUnchanged(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 200, Seed: 1})
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		r := rules.Generate(rng, env, 3)
		before := r.String()
		if g, ok := Ground(r, tbl, rng, env); ok {
			if g.String() == "" {
				t.Fatal("empty grounded rule")
			}
		}
		if r.String() != before {
			t.Fatalf("Ground modified its input: %s -> %s", before, r.String())
		}
	}
}
