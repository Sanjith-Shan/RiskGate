// Command export writes the offline training data: every transaction's
// feature vector, computed by replaying history through the same feature
// engine the service runs, then encoded exactly as the service encodes it.
// Python trains on this file and never recomputes a feature.
//
//	go run ./cmd/export                       # real IEEE-CIS data in data/raw
//	go run ./cmd/export -synthetic            # cmd/synth's data in data/synth
//	go run ./cmd/export -state sketch -out data/export_sketch   # experiment 4
//
// Output, in -out (default data/export_real, or data/export_synthetic):
//
//   - export.csv: one row per transaction in (TransactionDT,
//     TransactionID) order. Columns: TransactionID, TransactionDT, split
//     (train, valid, test), month (0-5), isFraud, then the model inputs in
//     encoder order, which begin with amount (the payment in dollars). Floats
//     are written with strconv's shortest round-trip form ('g', -1), so
//     parsing them gives back the exact float64 the service computes; a
//     missing value is an empty cell. Categorical inputs are integer codes
//     from encoder.json. CSV keeps the pipeline dependency-free; pandas reads
//     the 590K-row file in a few seconds.
//   - encoder.json: the schema.Encoder, fitted on the train months only, so
//     category codes carry no information from validation or test.
//   - features.json: model feature names, which are categorical, which are
//     raw fields and which are velocity features, the split rule, row and
//     fraud counts, and whether the data is SYNTHETIC. The "LightGBM on raw
//     columns only" baseline selects raw_features by name from the one
//     export; nothing is exported twice.
//   - features.table: the same rows, unencoded, as the backtester's columnar
//     table (internal/backtest). risk_score is NaN until a trained model
//     fills it in.
//   - test_replay.jsonl: the test month as Clearinghouse replay events (see
//     data.ReplayEvent), one JSON object per line.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

func main() {
	log.SetFlags(0)
	var cfg config
	dataDir := flag.String("data", "data", "data directory (raw/, synth/, cache/ live here)")
	synthetic := flag.Bool("synthetic", false, "use cmd/synth's SYNTHETIC data instead of IEEE-CIS")
	flag.StringVar(&cfg.out, "out", "", "output directory (default data/export_real, or data/export_synthetic)")
	flag.StringVar(&cfg.state, "state", "exact", "velocity state: exact, bucketed, or sketch")
	flag.Parse()

	cfg.paths = data.RealPaths(*dataDir)
	if *synthetic {
		cfg.paths = data.SyntheticPaths(*dataDir)
	}
	if cfg.out == "" {
		cfg.out = filepath.Join(*dataDir, "export_real")
		if *synthetic {
			cfg.out = filepath.Join(*dataDir, "export_synthetic")
		}
	}
	if _, err := os.Stat(cfg.paths.Transactions); err != nil {
		if _, cerr := os.Stat(cfg.paths.Cache); cerr != nil {
			hint := "run scripts/fetch_data.sh, or use -synthetic after go run ./cmd/synth"
			if *synthetic {
				hint = "run go run ./cmd/synth first"
			}
			log.Fatalf("export: no data at %s (%s)", cfg.paths.Transactions, hint)
		}
	}

	start := time.Now()
	sum, err := run(cfg, os.Stdout)
	if err != nil {
		log.Fatal("export: ", err)
	}
	fmt.Printf("%sdone in %v: load %v, fit %v, replay+write %v\n", sum.label(),
		time.Since(start).Round(time.Millisecond), sum.load.Round(time.Millisecond),
		sum.fit.Round(time.Millisecond), sum.write.Round(time.Millisecond))
}
