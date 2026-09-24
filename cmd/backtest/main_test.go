package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestDifftest(t *testing.T) {
	out := runOK(t, "difftest", "-rules", "200", "-rows", "1500", "-edge", "0.1", "-missing", "0.3")
	for _, want := range []string{"rules generated:   200", "rows:              1,500", "disagreements:     0 (in 0 rules)", "SYNTHETIC"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestSynthRunSweepLabelDelay(t *testing.T) {
	dir := t.TempDir()
	table := filepath.Join(dir, "t.rgt")
	runOK(t, "synth", "-out", table, "-rows", "20000")
	current := filepath.Join(dir, "rules.txt")
	if err := os.WriteFile(current, []byte("block if :risk_score: >= 90\nreview if :risk_score: >= 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runOK(t, "run", "-table", table, "-current", current, "-rule", "block if :risk_score: >= 70")
	if !strings.Contains(out, "this rule would have blocked") || !strings.Contains(out, "overlap:") {
		t.Errorf("run output:\n%s", out)
	}
	out = runOK(t, "run", "-table", table, "-current", current, "-rule", "review if :risk_score: >= 95", "-maturity-days", "0")
	if !strings.Contains(out, "warning: This rule would not change any decision") {
		t.Errorf("unreachable not reported:\n%s", out)
	}
	out = runOK(t, "run", "-table", table, "-rule", "block if :amount: > 100", "-json", "-from-day", "10", "-to-day", "100")
	var r map[string]any
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &r); err != nil || r["summary"] == "" {
		t.Errorf("run -json: %v", err)
	}

	out = runOK(t, "sweep", "-table", table, "-current", current, "-given-current", "-step", "10")
	if !strings.Contains(out, "risk_score >=") || strings.Count(out, "\n") < 11 {
		t.Errorf("sweep output:\n%s", out)
	}
	runOK(t, "sweep", "-table", table, "-json")

	out = runOK(t, "labeldelay", "-table", table, "-rule", "block if :risk_score: >= 70")
	if !strings.Contains(out, "SIMULATED") || !strings.Contains(out, "matured, truth") {
		t.Errorf("labeldelay output:\n%s", out)
	}
	runOK(t, "labeldelay", "-table", table, "-rule", "block if :risk_score: >= 70", "-json")
}

func TestBench(t *testing.T) {
	out := runOK(t, "bench", "-rows", "3000", "-reps", "1")
	for _, want := range []string{"machine:", "GOMAXPROCS=", "50-rule set", "row-at-a-time", "Backtester.Run", "indicative", "agree on all 3,000 rows"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	runOK(t, "bench", "-rows", "1000", "-reps", "1", "-json")
}

func TestErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"run", "-table", "synthetic", "-rows", "10"},                                // no -rule
		{"run", "-rows", "10", "-rule", "block if :amont: > 5"},                      // unknown attribute
		{"run", "-rows", "10", "-rule", "block if :amount: > 5\nblock if true"},      // two rules
		{"run", "-rows", "10", "-rule", "block if true", "-current", "/nonexistent"}, // missing file
		{"run", "-table", "/nonexistent", "-rule", "block if true"},
		{"synth"},
		{"sweep", "-bogus"},
		{"labeldelay", "-rows", "100", "-rule", "block if true", "-maturity-days", "0"},
	} {
		var out bytes.Buffer
		if err := run(args, &out); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	var out bytes.Buffer
	err := run([]string{"run", "-rows", "10", "-rule", "block if :amont: > 5"}, &out)
	if err == nil || !strings.Contains(err.Error(), "Did you mean :amount:?") {
		t.Errorf("rule errors should be rendered for analysts: %v", err)
	}
}

func TestFormatting(t *testing.T) {
	if commas(1234567) != "1,234,567" || commas(-1234) != "-1,234" || commas(12) != "12" {
		t.Error("commas")
	}
	half := 0.5
	if pct(nil) != "-" || pct(&half) != "50.0%" || dollars(1234.4) != "$1,234" {
		t.Error("pct/dollars")
	}
	if !strings.Contains(machine(), "GOMAXPROCS") {
		t.Error("machine")
	}
}

func TestFlagAndRuleStats(t *testing.T) {
	dir := t.TempDir()
	table := filepath.Join(dir, "t.rgt")
	runOK(t, "synth", "-out", table, "-rows", "20000")
	set := filepath.Join(dir, "set.rules")
	src := "allow if :amount: < 5\nblock if :amount: > 900\nreview if :risk_score: >= 80\nshadow block if :amount: > 1\n"
	if err := os.WriteFile(set, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	csvPath := filepath.Join(dir, "flags.csv")
	out := runOK(t, "flag", "-table", table, "-rules", set, "-out", csvPath)
	if !strings.Contains(out, "20,000 rows") {
		t.Errorf("flag output:\n%s", out)
	}
	b, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if lines[0] != "TransactionID,flagged" || len(lines) != 20001 {
		t.Fatalf("csv header %q, %d lines", lines[0], len(lines))
	}

	// The CSV must agree with the rule set evaluated row by row: flagged
	// exactly when the decision is block or review (shadow rules never count).
	tbl, err := backtest.Load(table, schema.Default())
	if err != nil {
		t.Fatal(err)
	}
	rs, err := loadRules(src)
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for i, d := range backtest.EvaluateRows(rs, tbl.Rows()) {
		want := "0"
		if d.Action == rules.Block || d.Action == rules.Review {
			want = "1"
			flagged++
		}
		if got := lines[i+1]; got != fmt.Sprintf("%d,%s", tbl.ID[i], want) {
			t.Fatalf("row %d: %q, want flagged %s", i, got, want)
		}
	}

	out = runOK(t, "rulestats", "-table", table, "-rules", set, "-split", "all", "-json")
	var s SetStat
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &s); err != nil {
		t.Fatal(err)
	}
	if s.Rows != 20000 || s.Flagged != flagged || len(s.Rules) != 4 || s.Rules[3].Action != "shadow block" {
		t.Errorf("rulestats: rows %d, flagged %d (want %d), %d rules", s.Rows, s.Flagged, flagged, len(s.Rules))
	}
	if s.Rules[3].Decided != 0 {
		t.Error("a shadow rule decided payments")
	}
	out = runOK(t, "rulestats", "-table", table, "-rules", set, "-split", "valid")
	if !strings.Contains(out, "split valid") {
		t.Errorf("rulestats output:\n%s", out)
	}
	for _, args := range [][]string{
		{"flag", "-table", table, "-rules", set},
		{"rulestats", "-table", table},
		{"rulestats", "-table", table, "-rules", set, "-split", "bogus"},
	} {
		var b bytes.Buffer
		if err := run(args, &b); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
}

// -lists replaces the built-in sample lists, so rules written against the
// service's rules/lists.json backtest as they will run.
func TestListsFlag(t *testing.T) {
	defer func() { ruleLists = rules.SampleLists() }()
	rule := `review if :purchaser_email_domain: in @only_here`
	var out bytes.Buffer
	if err := run([]string{"run", "-rows", "100", "-rule", rule}, &out); err == nil || !strings.Contains(err.Error(), "@only_here") {
		t.Fatalf("unknown list accepted: %v", err)
	}
	lists := filepath.Join(t.TempDir(), "lists.json")
	if err := os.WriteFile(lists, []byte(`{"only_here": ["gmail.com"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, "run", "-rows", "100", "-lists", lists, "-rule", rule)
	if err := run([]string{"run", "-rows", "100", "-lists", filepath.Join(t.TempDir(), "missing.json"), "-rule", rule}, &out); err == nil {
		t.Error("a missing lists file was accepted")
	}
}
