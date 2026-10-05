package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// k2: pipeline processes are killed at random moments (SIGKILL on Linux,
// TerminateProcess on Windows: no checkpoint, no commit, no goodbye to the
// group) while the full replay streams through, and restarted. Every
// payment must still get exactly one decision, the decisions must match the
// offline pipeline bit for bit, and the snapshots' own counters must show
// every entity event applied exactly once.
func k2(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("k2", flag.ExitOnError)
	var c common
	c.register(fs, "k2")
	kills := fs.Int("kills", 30, "processes to kill")
	minGap := fs.Duration("min-gap", 4*time.Second, "shortest time between kills")
	maxGap := fs.Duration("max-gap", 12*time.Second, "longest time between kills")
	maxDelay := fs.Duration("max-restart-delay", 2*time.Second, "a killed process restarts after a uniform delay up to this")
	rate := fs.Float64("rate", 1500, "payments per second (so the replay lasts long enough for the kills)")
	seed := fs.Uint64("seed", 1, "random seed for kill times and victims")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.members < 1 {
		c.members = 3
	}
	if err := c.setup("k2"); err != nil {
		return err
	}
	e := &c.env
	txns, err := loadTxns(e.data, c.limit)
	if err != nil {
		return err
	}
	prov := provenance()
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
	time.Sleep(5 * time.Second)

	sampler := startSampler()
	start := time.Now()
	var produceErr error
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		_, produceErr = e.run(ctx, "riskgate", append([]string{"stream", "produce", "-data", e.data, "-limit", strconv.Itoa(len(txns)),
			"-rate", strconv.FormatFloat(*rate, 'f', -1, 64), "-heartbeat-every", "1000"}, e.kafkaArgs()...)...)
	}()

	type killEvent struct {
		Victim     string  `json:"victim"`
		AtS        float64 `json:"at_s"`
		DownMs     int64   `json:"down_ms"`
		Log        string  `json:"log"`
		restarted  *member
		restartLog string
	}
	var events []*killEvent
	rng := rand.New(rand.NewPCG(*seed, 0x6b32))
	var mu sync.Mutex
	for len(events) < *kills {
		gap := *minGap + time.Duration(rng.Int64N(int64(*maxGap-*minGap)+1))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-produced:
		case <-time.After(gap):
		}
		select {
		case <-produced:
		default:
			m := members[rng.IntN(len(members))]
			ev := &killEvent{Victim: m.name, AtS: time.Since(start).Seconds()}
			m.kill()
			down := time.Duration(rng.Int64N(int64(*maxDelay) + 1))
			time.Sleep(down)
			ev.DownMs = down.Milliseconds()
			m.restarts++
			if err := e.start(m); err != nil {
				return err
			}
			ev.Log = m.logPath
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			fmt.Printf("%6.1fs killed %s, restarted after %v (%d/%d)\n", ev.AtS, m.name, down, len(events), *kills)
			continue
		}
		break // production finished before all kills: stop killing
	}
	<-produced
	if produceErr != nil {
		return produceErr
	}
	fmt.Printf("produced; %d kills; waiting for every decision\n", len(events))
	if _, err := stream.WaitForDecisions(ctx, []string{e.brokers}, e.prefix+".decisions", int64(len(txns)), 0); err != nil {
		return err
	}
	// Re-sent decisions after the last restarts can still be on the way.
	if _, err := stream.WaitForDecisions(ctx, []string{e.brokers}, e.prefix+".decisions", 0, 20*time.Second); err != nil {
		return err
	}
	decidedAt := time.Since(start)
	prov.Load.RunCPUPctMean, prov.Load.RunCPUPctMax = sampler.finish()
	for _, m := range members {
		if err := m.quit(60 * time.Second); err != nil {
			fmt.Println("warning:", err)
		}
	}

	// Restart times, from each restarted process's own log.
	type restart struct {
		Victim       string           `json:"victim"`
		DownMs       int64            `json:"down_ms"`
		FirstBatchMs map[string]int64 `json:"first_batch_ms_after_start"`
		RestoreMs    []int64          `json:"restore_ms"`
	}
	var restarts []restart
	var firstAll, restoreAll []int64
	for _, ev := range events {
		le := readLogEvents(ev.Log)
		restarts = append(restarts, restart{ev.Victim, ev.DownMs, le.FirstBatchMs, le.RestoreMs})
		// The process is back when its slowest stage has processed a batch.
		var worst int64 = -1
		for _, ms := range le.FirstBatchMs {
			worst = max(worst, ms)
		}
		if worst >= 0 {
			firstAll = append(firstAll, worst)
		}
		restoreAll = append(restoreAll, le.RestoreMs...)
	}

	fmt.Println("verifying")
	v, err := e.verify(ctx, len(txns), c.export, txns)
	if err != nil {
		return err
	}
	res := map[string]any{
		"payments": len(txns), "kills": len(events), "kill_events": events, "restarts": restarts,
		"restart_to_all_stages_processing_ms": dist(firstAll), "partition_restore_ms": dist(restoreAll),
		"all_decided_s": decidedAt.Seconds(), "verification": v,
		"lost_payments": v.Lost, "conflicting_resends": v.Decisions.Conflicts,
	}
	if v.State != nil {
		res["entity_events_applied_minus_expected"] = int64(v.State.AppliedTotal) - int64(v.EntityExpected)
		res["payments_decided_in_state_minus_expected"] = int64(v.State.Decided) - int64(len(txns))
	}
	r := row{Experiment: "k2", Run: c.runID, Provenance: prov, Result: res,
		Config: map[string]any{"members": c.members, "layout": e.layout, "rate": *rate, "kills": *kills, "min_gap": minGap.String(),
			"max_gap": maxGap.String(), "max_restart_delay": maxDelay.String(), "seed": *seed, "static_membership": true,
			"session_timeout": e.session.String(), "checkpoint_every": e.every.String()},
		Quotable: true, Note: "loss and double-count results are load-independent; restart times are quotable only if provenance.load.quiet",
	}
	if err := appendRow(c.results, r); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"kills": len(events), "lost": v.Lost, "conflicts": v.Decisions.Conflicts,
		"duplicates": v.Decisions.Duplicates, "offline_ok": v.OfflineOK, "feature_differ": v.FeatureDiffer,
		"state": v.State, "entity_expected": v.EntityExpected, "restart_ms": dist(firstAll)}, "", "  ")
	fmt.Println(string(b))
	return nil
}

// distribution summary
type summary struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	Max    float64 `json:"max"`
}

func dist(xs []int64) summary {
	if len(xs) == 0 {
		return summary{}
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	q := func(p float64) float64 { return float64(s[int(p*float64(len(s)-1)+0.5)]) }
	return summary{N: len(s), Min: float64(s[0]), Median: q(0.5), P90: q(0.9), Max: float64(s[len(s)-1])}
}
