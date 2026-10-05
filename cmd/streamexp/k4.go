package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// k4: partitions move between members while the replay streams through.
// Members use dynamic group membership, so a member that leaves cleanly
// hands its partitions over at once (checkpoint, commit, leave) and one that
// dies hands them over after the session timeout, from its last snapshot.
// The schedule scales out, scales in and loses a member mid-replay. The
// decisions must match the offline pipeline bit for bit regardless.
func k4(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("k4", flag.ExitOnError)
	var c common
	c.register(fs, "k4")
	rate := fs.Float64("rate", 2000, "payments per second")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.setup("k4"); err != nil {
		return err
	}
	e := &c.env
	txns, err := loadTxns(e.data, c.limit)
	if err != nil {
		return err
	}
	span := time.Duration(float64(len(txns)) / *rate * float64(time.Second))
	prov := provenance()
	prov.Load = baseline(10 * time.Second)
	if err := e.topics(ctx); err != nil {
		return err
	}
	all := map[string]*member{}
	mk := func(name string, port int) *member {
		m := &member{name: name, port: port, stages: "route,aggregate,join,labels"}
		all[name] = m
		return m
	}
	defer func() {
		for _, m := range all {
			m.kill()
		}
	}()
	if err := e.start(mk("a", 19500)); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)

	// The schedule, as fractions of the replay's length.
	type step struct {
		At     float64
		Action string
		Member string
	}
	plan := []step{
		{0.15, "start", "b"},
		{0.30, "start", "c"},
		{0.45, "quit", "a"}, // scale in: clean handover
		{0.60, "kill", "b"}, // failure: handover after the session timeout, from snapshots
		{0.75, "start", "d"},
	}
	type done struct {
		Action, Member string
		AtS            float64
	}
	var log []done
	sampler := startSampler()
	start := time.Now()
	produced := make(chan struct{})
	var produceErr error
	go func() {
		defer close(produced)
		_, produceErr = e.run(ctx, "riskgate", append([]string{"stream", "produce", "-data", e.data, "-limit", strconv.Itoa(len(txns)),
			"-rate", strconv.FormatFloat(*rate, 'f', -1, 64), "-heartbeat-every", "1000"}, e.kafkaArgs()...)...)
	}()
	ports := map[string]int{"b": 19501, "c": 19502, "d": 19503}
	for _, s := range plan {
		at := time.Duration(s.At * float64(span))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(start.Add(at))):
		}
		switch s.Action {
		case "start":
			if err := e.start(mk(s.Member, ports[s.Member])); err != nil {
				return err
			}
		case "quit":
			if err := all[s.Member].quit(60 * time.Second); err != nil {
				fmt.Println("warning:", err)
			}
		case "kill":
			all[s.Member].kill()
		}
		log = append(log, done{s.Action, s.Member, time.Since(start).Seconds()})
		fmt.Printf("%6.1fs %s %s\n", time.Since(start).Seconds(), s.Action, s.Member)
	}
	<-produced
	if produceErr != nil {
		return produceErr
	}
	if _, err := stream.WaitForDecisions(ctx, []string{e.brokers}, e.prefix+".decisions", int64(len(txns)), 0); err != nil {
		return err
	}
	if _, err := stream.WaitForDecisions(ctx, []string{e.brokers}, e.prefix+".decisions", 0, 15*time.Second); err != nil {
		return err
	}
	decidedAt := time.Since(start)
	prov.Load.RunCPUPctMean, prov.Load.RunCPUPctMax = sampler.finish()

	// Decision latency per 5-second window of decision time: a handover
	// shows up as a spike.
	type window struct {
		T     float64 `json:"t_s"`
		N     int     `json:"decisions"`
		MaxMs float64 `json:"max_latency_ms"`
		P99Ms float64 `json:"p99_latency_ms"`
	}
	lat := map[int][]float64{}
	err = stream.ReadTopic(ctx, []string{e.brokers}, e.prefix+".decisions", func(r *kgo.Record) error {
		var l struct {
			At        int64   `json:"at_ms"`
			LatencyUs float64 `json:"latency_us"`
		}
		if err := json.Unmarshal(r.Value, &l); err != nil {
			return err
		}
		w := int(time.UnixMilli(l.At).Sub(start).Seconds()) / 5
		lat[w] = append(lat[w], l.LatencyUs/1000)
		return nil
	})
	if err != nil {
		return err
	}
	var windows []window
	for w := range lat {
		xs := lat[w]
		slices.Sort(xs)
		windows = append(windows, window{float64(w * 5), len(xs), xs[len(xs)-1], xs[int(0.99*float64(len(xs)-1))]})
	}
	slices.SortFunc(windows, func(a, b window) int { return int(a.T - b.T) })
	for _, m := range all {
		_ = m.quit(60 * time.Second)
	}
	logs := map[string]logEvents{}
	var restores []int64
	for name, m := range all {
		le := readLogEvents(m.logPath)
		logs[name] = le
		restores = append(restores, le.RestoreMs...)
	}
	fmt.Println("verifying")
	v, err := e.verify(ctx, len(txns), c.export, txns)
	if err != nil {
		return err
	}
	res := map[string]any{
		"payments": len(txns), "events": log, "member_logs": logs, "partition_restore_ms": dist(restores),
		"latency_windows": windows, "all_decided_s": decidedAt.Seconds(), "verification": v,
	}
	r := row{Experiment: "k4", Run: c.runID, Provenance: prov, Result: res,
		Config: map[string]any{"layout": e.layout, "rate": *rate, "plan": plan, "static_membership": false,
			"session_timeout": e.session.String(), "checkpoint_every": e.every.String()},
		Quotable: true, Note: "mismatch counts are load-independent; latency windows are quotable only if provenance.load.quiet",
	}
	if err := appendRow(c.results, r); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"lost": v.Lost, "offline_ok": v.OfflineOK, "feature_differ": v.FeatureDiffer,
		"state": v.State, "entity_expected": v.EntityExpected, "restores": dist(restores)}, "", "  ")
	fmt.Println(string(b))
	return nil
}
