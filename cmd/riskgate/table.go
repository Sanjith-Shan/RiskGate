package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// table builds the backtester's feature table: every transaction replayed
// in event order through the same feature engine the service runs (score
// before update), scored by the same model, and stored column-wise with its
// label. A backtest over it therefore sees exactly the features and
// risk_score the service would have computed.
func table(args []string) error {
	fs := flag.NewFlagSet("table", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory (raw/, synth/, cache/)")
	synthetic := fs.Bool("synthetic", false, "use cmd/synth's SYNTHETIC data instead of IEEE-CIS")
	modelDir := fs.String("model", "", "model directory; without one risk_score is missing in the table")
	state := fs.String("state", "exact", "velocity state: exact, bucketed or sketch (match the service's -state)")
	out := fs.String("out", "", "output file (default <data>/cache/table.rgt, or table_synthetic.rgt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := data.RealPaths(*dataDir)
	name := "table.rgt"
	if *synthetic {
		paths, name = data.SyntheticPaths(*dataDir), "table_synthetic.rgt"
	}
	if *out == "" {
		*out = filepath.Join(*dataDir, "cache", name)
	}

	start := time.Now()
	ds, err := data.Load(paths)
	if err != nil {
		return err
	}
	cat := schema.Default()
	var scorer *model.Scorer
	if *modelDir != "" {
		if scorer, err = model.LoadScorer(*modelDir, cat); err != nil {
			return err
		}
	}
	// Replay is single-threaded, so the state needs no locking; the
	// features are the same whichever concurrency wrapper the service uses.
	var st features.State
	switch *state {
	case "exact":
		st = features.NewExact(0)
	case "bucketed":
		st = features.NewBucketed(features.BucketedConfig{})
	case "sketch":
		st = features.NewSketch(features.SketchConfig{})
	default:
		return fmt.Errorf("unknown -state %q", *state)
	}
	engine, err := features.NewEngine(cat, st)
	if err != nil {
		return err
	}
	risk := cat.MustLookup(schema.RiskScoreField.Name).Slot
	b := backtest.NewBuilder(cat, len(ds.Txns))
	err = features.Replay(engine, ds.Txns, func(t *data.Txn, row schema.Row) error {
		if scorer != nil {
			score, _, _ := scorer.Score(row)
			row.Num[risk] = float64(score)
		}
		b.Append(row, backtest.Meta{ID: t.ID, DT: t.DT, Amount: t.Amount, Fraud: t.IsFraud})
		return nil
	})
	if err != nil {
		return err
	}
	t := b.Table()
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := t.Save(*out); err != nil {
		return err
	}
	label := ""
	if ds.Synthetic {
		label = "SYNTHETIC "
	}
	fmt.Printf("wrote %s: %d %srows, state %s, model %v, in %v\n", *out, t.N, label, *state, scorer != nil, time.Since(start).Round(time.Millisecond))
	return nil
}
