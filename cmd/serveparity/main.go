// Command serveparity is experiment 2's feature-parity check through the
// HTTP service: the features the running service computes for a payment
// must be the features the offline export computed for the same
// transaction, to the bit.
//
// It has three steps, run in order (scripts/experiments/exp2.sh drives them):
//
//	serveparity replayfile -data data -out data/replay_all.jsonl
//	    every transaction (all months, not just the test month) as an assess
//	    request, in (TransactionDT, TransactionID) order, in the risk_fields
//	    format of test_replay.jsonl. Labels are left out: the service never
//	    sees them. The file is row-level data and belongs in data/ (ignored
//	    by git), never in results/.
//
//	serveparity send -input data/replay_all.jsonl -url http://127.0.0.1:8080/v1/assess
//	    posts every line, one at a time and in order, so the service's
//	    velocity state sees exactly the history the offline replay saw.
//
//	serveparity compare -log var/exp2/decisions.jsonl -data data \
//	    -export data/export_real -model models/ieee -json results/exp2/feature_parity.json
//	    streams the decision log, a fresh offline replay (features.Replay,
//	    the path cmd/export runs) and export.csv side by side and compares
//	    every row: the full catalog feature vector against the offline row,
//	    the encoded model input against export.csv, and the logged raw score
//	    and risk_score against the offline Scorer. Only aggregates are
//	    written; row-level examples go to stderr.
//
// compare exits 1 if any row differs.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: serveparity replayfile|send|compare [flags]")
		os.Exit(2)
	}
	var err error
	ok := true
	switch os.Args[1] {
	case "replayfile":
		err = replayFile(os.Args[2:])
	case "send":
		err = send(os.Args[2:])
	case "compare":
		ok, err = compare(os.Args[2:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "serveparity: unknown command %q (want replayfile, send or compare)\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "serveparity:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}
