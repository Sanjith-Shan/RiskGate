package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
)

// audit replays every decision in a decision log against the rule-set
// version and model that made it, and reports any that come out
// differently. It returns false if any did.
func audit(args []string, stdout io.Writer) (bool, error) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	logPath := fs.String("log", "var/decisions.jsonl", "decision log to replay")
	history := fs.String("rules-history", "var/rules-history", "rule history directory the service wrote")
	modelDir := fs.String("model", "", "model directory the service ran (required if decisions were scored)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	cat := schema.Default()
	var scorer *model.Scorer
	var err error
	if *modelDir != "" {
		if scorer, err = model.LoadScorer(*modelDir, cat); err != nil {
			return false, err
		}
	}
	sets, err := service.LoadRuleHistory(*history, cat)
	if err != nil {
		return false, err
	}
	if len(sets) == 0 {
		return false, errors.New("no rule-set versions in " + *history)
	}
	f, err := os.Open(*logPath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	rep, err := service.Audit(f, cat, scorer, sets)
	if err != nil {
		return false, err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return rep.OK(), enc.Encode(rep)
	}
	fmt.Fprintln(stdout, rep)
	for _, m := range rep.Examples {
		fmt.Fprintf(stdout, "  line %d %s: %s logged %s, replayed %s\n", m.Line, m.AssessmentID, m.Field, m.Logged, m.Replayed)
	}
	if rep.OK() {
		fmt.Fprintln(stdout, "OK: every decision replayed identically")
	}
	return rep.OK(), nil
}
