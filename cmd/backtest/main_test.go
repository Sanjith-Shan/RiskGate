package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, want := range []string{"machine:", "GOMAXPROCS=", "50-rule set", "row-at-a-time", "Backtester.Run", "indicative"} {
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
