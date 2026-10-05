// Command streamexp runs the stream pipeline's experiments against a real
// Kafka broker and appends one JSON row per run to results/stream/*.jsonl,
// each stamped with the commit, the machine and how busy it was.
//
//	streamexp k1      parity: every payment through Kafka against the offline pipeline and the HTTP path
//	streamexp k2      crashes: pipeline processes killed at random points; nothing lost or counted twice
//	streamexp k3      lag: consumer lag and decision latency at increasing produce rates
//	streamexp k4      rebalance: members join, leave and die mid-replay; state handed over
//	streamexp naive   negative control, offline: what partitioning by card alone would compute
//	streamexp skew    how entity events spread over the aggregator partitions (hot keys)
//	streamexp httplog the HTTP path's decision log for the same payments, for k1's comparison
//
// Row-level files (decision logs, snapshots, process logs) go under build/stream
// (ignored by git and kept off any file sync), never results/. Start Kafka first: scripts/kafka_local.sh start.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: streamexp k1|k2|k3|k4|naive|skew|httplog [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "k1":
		err = k1(ctx, os.Args[2:])
	case "k2":
		err = k2(ctx, os.Args[2:])
	case "k3":
		err = k3(ctx, os.Args[2:])
	case "k4":
		err = k4(ctx, os.Args[2:])
	case "naive":
		err = naive(os.Args[2:])
	case "skew":
		err = skew(os.Args[2:])
	case "httplog":
		err = httplog(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown experiment %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "streamexp:", err)
		os.Exit(1)
	}
}

// common flags of the Kafka experiments.
type common struct {
	env     env
	results string
	export  string
	limit   int
	runID   string
	members int
	payP    int
	entP    int
	partP   int
	decP    int
}

func (c *common) register(fs *flag.FlagSet, exp string) {
	fs.StringVar(&c.env.bin, "bin", "build/bin", "directory with the riskgate and serveparity binaries")
	fs.StringVar(&c.env.brokers, "brokers", "127.0.0.1:19092", "Kafka brokers")
	fs.StringVar(&c.env.model, "model", "models/ieee", "model directory")
	fs.StringVar(&c.env.rules, "rules", "rules/default.rules", "rule set")
	fs.StringVar(&c.env.lists, "lists", "rules/lists.json", "named lists")
	fs.StringVar(&c.env.data, "data", "data", "data directory (cache/ieee.rgc)")
	fs.DurationVar(&c.env.session, "session-timeout", 10*time.Second, "group session timeout")
	fs.DurationVar(&c.env.every, "checkpoint-every", 5*time.Second, "checkpoint interval of the stateful stages")
	fs.StringVar(&c.export, "export", "build/export_check", "cmd/export directory of the same data (export.csv, encoder.json)")
	fs.StringVar(&c.results, "results", filepath.Join("results", "stream", exp+".jsonl"), "results file (one JSON row per run)")
	fs.IntVar(&c.limit, "limit", 0, "replay only the first N payments (0: all 590,540)")
	fs.StringVar(&c.runID, "run", "", "run id (default: experiment and time)")
	fs.IntVar(&c.members, "members", 1, "pipeline processes, each running every stage")
	fs.IntVar(&c.payP, "payments-partitions", 8, "payments partitions")
	fs.IntVar(&c.entP, "entity-partitions", 8, "entity-events partitions")
	fs.IntVar(&c.partP, "parts-partitions", 8, "parts partitions")
	fs.IntVar(&c.decP, "decision-partitions", 8, "decisions partitions")
}

func (c *common) setup(exp string) error {
	if c.runID == "" {
		c.runID = exp + "-" + time.Now().UTC().Format("20060102T150405")
	}
	c.env.prefix = "rg-" + c.runID
	c.env.work = filepath.Join("build", "stream", c.runID)
	c.env.layout = stream.Layout{Payments: int32(c.payP), EntityEvents: int32(c.entP), Parts: int32(c.partP), Decisions: int32(c.decP), Disputes: 1}
	if err := os.MkdirAll(c.env.work, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(c.env.exe("riskgate")); err != nil {
		return fmt.Errorf("build the binaries first: go build -o %s/ ./cmd/...", c.env.bin)
	}
	return nil
}

func loadTxns(dir string, limit int) ([]data.Txn, error) {
	ds, err := data.Load(data.RealPaths(dir))
	if err != nil {
		return nil, err
	}
	if ds.Synthetic {
		return nil, errors.New("refusing: the dataset is SYNTHETIC")
	}
	if limit > 0 && limit < len(ds.Txns) {
		return ds.Txns[:limit], nil
	}
	return ds.Txns, nil
}

// row is one result line.
type row struct {
	Experiment string     `json:"experiment"`
	Run        string     `json:"run"`
	Provenance Provenance `json:"provenance"`
	Config     any        `json:"config"`
	Result     any        `json:"result"`
	Quotable   bool       `json:"quotable"`
	Note       string     `json:"note,omitempty"`
}
