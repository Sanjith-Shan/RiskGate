package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// k1: every IEEE-CIS payment replayed through Kafka. The decisions topic is
// read back and compared with the offline pipeline (features bit for bit
// against features.Replay and export.csv, scores against the Scorer, via
// cmd/serveparity compare, the same check experiment 2 ran on the HTTP
// service), replayed with `riskgate audit`, and, given an HTTP-path decision
// log of the same payments, compared with it line by line.
func k1(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("k1", flag.ExitOnError)
	var c common
	c.register(fs, "k1")
	httpLog := fs.String("http-log", "", "the HTTP path's decision log for the same payments (streamexp httplog), to compare with")
	arrival := fs.Bool("arrival-order", false, "negative control: aggregators apply events in arrival order (no watermarks)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exp := "k1"
	if *arrival {
		exp = "k1-arrival-order"
	}
	if err := c.setup(exp); err != nil {
		return err
	}
	e := &c.env
	if *arrival {
		e.extra = append(e.extra, "-arrival-order")
	}
	txns, err := loadTxns(e.data, c.limit)
	if err != nil {
		return err
	}
	prov := provenance()
	fmt.Printf("k1 %s: %d payments, %d members; measuring background load\n", c.runID, len(txns), c.members)
	prov.Load = baseline(10 * time.Second)
	if err := e.topics(ctx); err != nil {
		return err
	}

	members := make([]*member, c.members)
	for i := range members {
		members[i] = &member{name: "m" + strconv.Itoa(i+1), instance: c.runID + "-m" + strconv.Itoa(i+1), port: 19500 + i, stages: "route,aggregate,join,labels"}
		if err := e.start(members[i]); err != nil {
			return err
		}
	}
	defer func() {
		for _, m := range members {
			m.kill()
		}
	}()
	time.Sleep(3 * time.Second) // join the groups before the first record

	sampler := startSampler()
	start := time.Now()
	out, err := e.run(ctx, "riskgate", append([]string{"stream", "produce", "-data", e.data, "-limit", strconv.Itoa(len(txns)), "-heartbeat-every", "1000"}, e.kafkaArgs()...)...)
	if err != nil {
		sampler.finish()
		return err
	}
	var produced map[string]any
	_ = json.Unmarshal(out, &produced)
	producedAt := time.Since(start)
	fmt.Printf("produced in %v; waiting for decisions\n", producedAt.Round(time.Millisecond))
	n, err := stream.WaitForDecisions(ctx, []string{e.brokers}, e.prefix+".decisions", int64(len(txns)), 0)
	if err != nil {
		sampler.finish()
		return err
	}
	decidedAt := time.Since(start)
	time.Sleep(3 * time.Second) // let any trailing re-sends land
	prov.Load.RunCPUPctMean, prov.Load.RunCPUPctMax = sampler.finish()

	metrics := map[string]any{}
	for _, m := range members {
		if mm, err := m.metrics(); err == nil {
			metrics[m.name] = mm
		}
	}
	for _, m := range members {
		if err := m.quit(60 * time.Second); err != nil {
			fmt.Println("warning:", err)
		}
	}
	logs := map[string]logEvents{}
	for _, m := range members {
		logs[m.name] = readLogEvents(m.logPath)
	}

	fmt.Println("verifying")
	v, err := e.verify(ctx, len(txns), c.export, txns)
	if err != nil {
		return err
	}
	fmt.Println("backtest from the topic against the offline table")
	bc, err := e.backtestCheck(ctx, txns, []string{e.rules, "rules/baseline.rules"})
	if err != nil {
		return err
	}
	res := map[string]any{
		"backtest_from_topic": bc,
		"payments":            len(txns), "decision_records": n, "produce": produced,
		"produce_s": producedAt.Seconds(), "all_decided_s": decidedAt.Seconds(),
		"decisions_per_s": float64(len(txns)) / decidedAt.Seconds(),
		"verification":    v, "member_metrics": metrics, "member_logs": logs,
	}
	if *httpLog != "" {
		d, err := compareLogs(filepath.Join(e.work, "stream_decisions.jsonl"), *httpLog)
		if err != nil {
			return err
		}
		res["vs_http_path"] = d
	}
	r := row{Experiment: exp, Run: c.runID, Provenance: prov, Result: res,
		Config: map[string]any{"members": c.members, "layout": e.layout, "model": e.model, "rules": e.rules,
			"checkpoint_every": e.every.String(), "arrival_order": *arrival},
		// Parity is a correctness result: machine load changes how long it
		// takes, not what it computes. Throughput is quotable only quiet.
		Quotable: true,
		Note:     "mismatch counts are load-independent; decisions_per_s is quotable only if provenance.load.quiet",
	}
	if err := appendRow(c.results, r); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"verification": v, "backtest_from_topic": bc, "vs_http": res["vs_http_path"], "decided_s": decidedAt.Seconds()}, "", "  ")
	fmt.Println(string(b))
	_ = os.Remove(filepath.Join(e.work, "stream_decisions.jsonl.tmp"))
	return nil
}
