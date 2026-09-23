package backtest

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// checkDecisions asserts that the vectorized decisions equal
// RuleSet.Evaluate on every row: the action, and the rule reported.
func checkDecisions(t *testing.T, rs *rules.RuleSet, tbl *Table) *Decisions {
	t.Helper()
	d, err := EvaluateRuleSet(rs, tbl, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := EvaluateRows(rs, tbl.Rows())
	bad := 0
	for i, w := range want {
		wantRule := int32(-1)
		if w.Rule != nil {
			wantRule = int32(w.Rule.Index)
		}
		if d.Action[i] != w.Action || d.Rule[i] != wantRule {
			if bad++; bad <= 5 {
				t.Errorf("row %d: vectorized %s by rule %d, Evaluate %s by rule %d", i, d.Action[i], d.Rule[i], w.Action, wantRule)
			}
		}
		if got := d.DecidingRule(i); got != w.Rule {
			t.Fatalf("row %d: DecidingRule %v, want %v", i, got, w.Rule)
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d rows differ", bad, tbl.N)
	}
	// The summary bitmaps must agree with the per-row actions.
	for i := range tbl.N {
		byRule := d.Rule[i] >= 0
		if d.Allowed.Get(i) != (d.Action[i] == rules.Allow && byRule) ||
			d.Blocked.Get(i) != (d.Action[i] == rules.Block) ||
			d.Reviewed.Get(i) != (d.Action[i] == rules.Review) {
			t.Fatalf("row %d: bitmaps disagree with action %s", i, d.Action[i])
		}
	}
	return d
}

func TestEvaluateRuleSetMatchesEvaluate(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 3000, Seed: 11, Edge: 0.05})
	rng := rand.New(rand.NewPCG(5, 6))
	sets := 60
	if testing.Short() || raceEnabled {
		sets = 10
	}
	for s := range sets {
		var lines []string
		for range 1 + rng.IntN(25) {
			lines = append(lines, rules.Generate(rng, env, rng.IntN(4)).String())
		}
		rs, err := rules.Load(strings.Join(lines, "\n"), env, uint64(s))
		if err != nil {
			t.Fatal(err)
		}
		checkDecisions(t, rs, tbl)
	}
}

func TestEvaluateRuleSetRealisticRules(t *testing.T) {
	env := testEnv()
	rs, err := rules.Load(RealisticRuleSet(), env, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rs.Rules); n != 50 {
		t.Fatalf("realistic rule set has %d rules, want 50", n)
	}
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 5000, Seed: 12})
	d := checkDecisions(t, rs, tbl)
	// A realistic set should decide some payments each way and leave most
	// alone, or it is not exercising Radar's ordering.
	if d.Allowed.Count() == 0 || d.Blocked.Count() == 0 || d.Reviewed.Count() == 0 {
		t.Errorf("allowed %d, blocked %d, reviewed %d", d.Allowed.Count(), d.Blocked.Count(), d.Reviewed.Count())
	}
}

func TestRadarOrder(t *testing.T) {
	env := testEnv()
	cat := env.Catalog
	b := NewBuilder(cat, 4)
	score := cat.MustLookup("risk_score").Slot
	email := cat.MustLookup("purchaser_email_domain").Slot
	for i, tc := range []struct {
		score float64
		email string
	}{{95, "gmail.com"}, {95, "yahoo.com"}, {60, "yahoo.com"}, {10, "yahoo.com"}} {
		r := cat.NewRow()
		r.Num[score], r.Str[email] = tc.score, tc.email
		b.Append(r, Meta{ID: int64(i), DT: int64(i)})
	}
	tbl := b.Table()
	rs, err := rules.Load(`review if :risk_score: >= 50
block if :risk_score: >= 90
block if :risk_score: >= 80
allow if :purchaser_email_domain: = "gmail.com"`, env, 1)
	if err != nil {
		t.Fatal(err)
	}
	d := checkDecisions(t, rs, tbl)
	want := []struct {
		a    rules.Action
		rule int32
	}{{rules.Allow, 3}, {rules.Block, 1}, {rules.Review, 0}, {rules.Allow, -1}}
	for i, w := range want {
		if d.Action[i] != w.a || d.Rule[i] != w.rule {
			t.Errorf("row %d: %s by %d, want %s by %d", i, d.Action[i], d.Rule[i], w.a, w.rule)
		}
	}
}

func TestCompileVectorRejectsForeignCatalog(t *testing.T) {
	env := testEnv()
	r, _ := rules.ParseRule(`block if :amount: > 5`)
	if err := rules.Check([]*rules.Rule{r}, env); err != nil {
		t.Fatal(err)
	}
	other := schema.New([]schema.Field{{Name: "x", Kind: schema.Number}, {Name: "amount", Kind: schema.String}})
	tbl := NewBuilder(other, 0).Table()
	if _, err := CompileVector(r.Cond, tbl); err == nil {
		t.Fatal("compiled against a table of another catalog")
	}
	unchecked, _ := rules.ParseRule(`block if :amount: > 5`)
	if _, err := CompileVector(unchecked.Cond, Synthetic(env.Catalog, SynthOptions{Rows: 1})); err == nil {
		t.Fatal("compiled an unchecked rule")
	}
}
