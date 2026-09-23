package rules

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func mustLoad(t testing.TB, src string) *RuleSet {
	t.Helper()
	rs, err := Load(src, testEnv(), 7)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return rs
}

// TestEvaluationOrder pins Radar's documented order: allow beats block and
// review regardless of where it appears in the file, block beats review,
// and no match means allow.
func TestEvaluationOrder(t *testing.T) {
	rs := mustLoad(t, strings.Join([]string{
		"review if :amount: > 100",
		"block if :amount: > 500",
		"block if :risk_score: > 90",
		"allow if :purchaser_email_domain: in @trusted_domains",
	}, "\n"))
	tests := []struct {
		name   string
		row    schema.Row
		action Action
		index  int // -1 for the default
	}{
		{"allow wins over block and review", row("amount", 1000, "purchaser_email_domain", "gmail.com"), Allow, 3},
		{"block wins over review", row("amount", 1000), Block, 1},
		{"first block in source order", row("amount", 1000, "risk_score", 95), Block, 1},
		{"second block", row("amount", 200, "risk_score", 95), Block, 2},
		{"review", row("amount", 200), Review, 0},
		{"default allow", row("amount", 50), Allow, -1},
		{"default allow when everything is missing", row(), Allow, -1},
	}
	for _, tt := range tests {
		d := rs.Evaluate(tt.row)
		idx := -1
		if d.Rule != nil {
			idx = d.Rule.Index
		}
		if d.Action != tt.action || idx != tt.index || d.Version != 7 {
			t.Errorf("%s: got %v by rule %d (v%d), want %v by rule %d", tt.name, d.Action, idx, d.Version, tt.action, tt.index)
		}
	}
}

func TestShadowRules(t *testing.T) {
	rs := mustLoad(t, strings.Join([]string{
		"allow if :purchaser_email_domain: in @trusted_domains",
		"shadow block if :amount: > 100",
		"shadow review if :risk_score: > 50",
		"shadow allow if :amount: > 0",
	}, "\n"))
	d := rs.Evaluate(row("amount", 500, "risk_score", 80, "purchaser_email_domain", "gmail.com"))
	if d.Action != Allow || d.Rule == nil || d.Rule.Index != 0 {
		t.Fatalf("shadow rules changed the decision: %+v", d)
	}
	var ids []int
	for _, r := range d.Shadow {
		ids = append(ids, r.Index)
		if !r.Shadow || r.ID == "" {
			t.Errorf("shadow match %+v", r)
		}
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Errorf("shadow matches = %v, want [1 2 3]", ids)
	}
	d = rs.Evaluate(row("amount", 500))
	if d.Action != Allow || d.Rule != nil {
		t.Errorf("a shadow block must not block: %+v", d)
	}
	if d = rs.Evaluate(row()); d.Shadow != nil {
		t.Errorf("no shadow match should leave Shadow nil, got %v", d.Shadow)
	}
}

func TestRuleIDsAreStable(t *testing.T) {
	a := mustLoad(t, "block if :amount: > 100\nreview if :risk_score: > 5")
	b := mustLoad(t, "# reformatted and reordered\nreview if (:risk_score: > 5)\n\n  block   if :amount:>100  # comment")
	if a.Rules[0].ID != b.Rules[1].ID || a.Rules[1].ID != b.Rules[0].ID {
		t.Error("formatting or order changed a rule's ID")
	}
	c := mustLoad(t, "shadow block if :amount: > 100\nreview if :amount: > 100")
	if c.Rules[0].ID == a.Rules[0].ID || c.Rules[1].ID == a.Rules[0].ID {
		t.Error("shadow flag or action must be part of the ID")
	}
	if cr := b.Rules[1]; cr.Line != 4 || cr.Text != "block   if :amount:>100" || cr.Action != Block {
		t.Errorf("compiled rule metadata = %+v", cr)
	}
}

func TestLoadReportsAllErrorsAndWarnings(t *testing.T) {
	_, err := Load("block if :amount: > 5 and :amount: < 3\nblock if :nope: > 1\nblok if true", testEnv(), 1)
	var ds Diagnostics
	if !errors.As(err, &ds) {
		t.Fatalf("err = %v", err)
	}
	if len(ds) != 3 || ds[0].Severity != SeverityWarning || ds[1].Span.Start.Line != 2 || ds[2].Span.Start.Line != 3 {
		t.Errorf("diagnostics:\n%s", ds.Render())
	}
}

func TestLint(t *testing.T) {
	tests := []struct {
		src  string
		want string // "" for no warning
	}{
		{"block if :amount: > 5 and :amount: < 3", "can never be true: :amount: > 5 and :amount: < 3"},
		{"block if :amount: >= 5 and :amount: < 5", "can never be true"},
		{"block if :amount: > 5 and :amount: <= 5", "can never be true"},
		{"block if 5 < :amount: and 3 > :amount:", "can never be true"},
		{"block if :amount: = 5 and :amount: = 6", "can never be true"},
		{"block if :amount: = 5 and :amount: > 5", "can never be true"},
		{"block if :amount: >= 5 and :amount: <= 5 and :amount: != 5", "can never be true"},
		{"block if :amount: = -1 and :risk_score: > 1 and :amount: >= 0", "can never be true: :amount: = -1 and :amount: >= 0"},
		{`block if :card_type: = "a" and :card_type: != "a"`, "can never be true"},
		{`block if :card_type: != "a" and :card_type: = "a"`, "can never be true"},
		{"block if :amount: > 5 or :amount: < 3", ""},
		{"block if :amount: >= 5 and :amount: <= 5", ""},
		{"block if :amount: = 5 and :amount: = 5", ""},
		{"block if :amount: > 5 and :risk_score: < 3", ""},
		{"block if not (:amount: > 5 and :amount: < 3) and :risk_score: > 1", ""},
		{`block if :card_type: = "a" and :card_network: = "b"`, ""},
		{"block if 2 > 1", "always true, so this rule will block every payment"},
		{"allow if :amount: > 1 or true", "always true, so this rule will allow every payment"},
		{"block if :amount: > 1 / 0", "never true"},
		{"block if not :amount: > 1 / 0", "never true"}, // UNKNOWN on every row, and not keeps it UNKNOWN
		{"block if not (:amount: > 1 / 0) or true", "always true"},
	}
	for _, tt := range tests {
		r := mustCheck(t, tt.src)
		ds := Lint([]*Rule{r})
		switch {
		case tt.want == "" && len(ds) > 0:
			t.Errorf("%s: unexpected warning %v", tt.src, ds)
		case tt.want != "" && (len(ds) != 1 || !strings.Contains(ds[0].Message(), tt.want)):
			t.Errorf("%s: warnings %v, want one containing %q", tt.src, ds, tt.want)
		}
	}
}

func TestZeroAllocs(t *testing.T) {
	env := testEnv()
	rng := rand.New(rand.NewPCG(5, 6))
	rows := make([]schema.Row, 16)
	for i := range rows {
		rows[i] = GenerateRow(rng, env.Catalog)
	}
	for i := range 300 {
		r := Generate(rng, env, i%6)
		cr, err := Compile(r)
		if err != nil {
			t.Fatal(err)
		}
		allocs := testing.AllocsPerRun(20, func() {
			for _, row := range rows {
				cr.Match(row)
			}
		})
		if allocs != 0 {
			t.Fatalf("%s allocates %.1f times per run", r, allocs)
		}
	}
}
