package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// k3: consumer lag and decision latency at increasing produce rates. The
// producer is open loop: each payment is stamped with its intended send
// time, and a decision's latency is measured from that stamp to the moment
// the joiner wrote it, so a pipeline falling behind shows up as latency and
// lag, not as a slower producer. Each rate is a fresh pipeline (new topics,
// empty state) replaying the first rate x duration payments.
func k3(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("k3", flag.ExitOnError)
	var c common
	c.register(fs, "k3")
	rates := fs.String("rates", "500,1000,2000,3000,4000,6000", "payments per second to try, in order")
	duration := fs.Duration("duration", 60*time.Second, "how long each rate is produced for")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.setup("k3"); err != nil {
		return err
	}
	all, err := loadTxns(c.env.data, 0)
	if err != nil {
		return err
	}
	runID := c.runID
	for _, rs := range strings.Split(*rates, ",") {
		rate, err := strconv.ParseFloat(strings.TrimSpace(rs), 64)
		if err != nil {
			return err
		}
		n := min(int(rate*duration.Seconds()), len(all))
		c.runID = fmt.Sprintf("%s-r%s", runID, rs)
		if err := c.setup("k3"); err != nil {
			return err
		}
		res, prov, err := k3Rate(ctx, &c, rate, n)
		if err != nil {
			return err
		}
		r := row{Experiment: "k3", Run: c.runID, Provenance: prov, Result: res,
			Config:   map[string]any{"members": c.members, "layout": c.env.layout, "rate": rate, "payments": n, "duration": duration.String()},
			Quotable: prov.Load.Quiet,
			Note:     "throughput and latency; quotable only when the background load was quiet (provenance.load)",
		}
		if err := appendRow(c.results, r); err != nil {
			return err
		}
		b, _ := json.Marshal(res)
		fmt.Printf("rate %v: %s\n", rate, b)
	}
	return nil
}

func k3Rate(ctx context.Context, c *common, rate float64, n int) (map[string]any, Provenance, error) {
	e := &c.env
	prov := provenance()
	prov.Load = baseline(10 * time.Second)
	if err := e.topics(ctx); err != nil {
		return nil, prov, err
	}
	members := make([]*member, c.members)
	for i := range members {
		members[i] = &member{name: "m" + strconv.Itoa(i+1), port: 19500 + i, stages: "route,aggregate,join,labels"}
		if err := e.start(members[i]); err != nil {
			return nil, prov, err
		}
	}
	defer func() {
		for _, m := range members {
			m.kill()
		}
	}()
	time.Sleep(5 * time.Second)

	cl, err := kgo.NewClient(kgo.SeedBrokers(e.brokers))
	if err != nil {
		return nil, prov, err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	decisions := e.prefix + ".decisions"
	decided := func() int64 {
		ends, err := adm.ListEndOffsets(ctx, decisions)
		if err != nil {
			return -1
		}
		var t int64
		ends.Each(func(o kadm.ListedOffset) { t += o.Offset })
		return t
	}

	sampler := startSampler()
	start := time.Now()
	produced := make(chan struct{})
	var produceOut []byte
	var produceErr error
	go func() {
		defer close(produced)
		produceOut, produceErr = e.run(ctx, "riskgate", append([]string{"stream", "produce", "-data", e.data, "-limit", strconv.Itoa(n),
			"-rate", strconv.FormatFloat(rate, 'f', -1, 64), "-heartbeat-every", "1000"}, e.kafkaArgs()...)...)
	}()
	type sample struct {
		T   float64 `json:"t_s"`
		Lag int64   `json:"lag"` // payments sent by the schedule but not yet decided
	}
	var samples []sample
	var maxLag int64
	var prodDone time.Duration
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		done := false
		select {
		case <-ctx.Done():
			return nil, prov, ctx.Err()
		case <-produced:
			if prodDone == 0 {
				prodDone = time.Since(start)
			}
		case <-tick.C:
		}
		el := time.Since(start)
		sent := min(int64(rate*el.Seconds()), int64(n))
		d := decided()
		lag := sent - d
		samples = append(samples, sample{el.Seconds(), lag})
		maxLag = max(maxLag, lag)
		if prodDone != 0 && d >= int64(n) {
			done = true
		}
		if el > time.Duration(float64(n)/rate*float64(time.Second))+5*time.Minute {
			return nil, prov, fmt.Errorf("rate %v: %d of %d decided 5 minutes after the end of production", rate, d, n)
		}
		if done {
			break
		}
	}
	allDecided := time.Since(start)
	prov.Load.RunCPUPctMean, prov.Load.RunCPUPctMax = sampler.finish()
	if produceErr != nil {
		return nil, prov, produceErr
	}

	// Latency of every decision, from its intended send time.
	h := hdrhistogram.New(1, 600_000_000, 3) // microseconds
	var count int
	err = stream.ReadTopic(ctx, []string{e.brokers}, decisions, func(r *kgo.Record) error {
		var l struct {
			LatencyUs float64 `json:"latency_us"`
		}
		if err := json.Unmarshal(r.Value, &l); err != nil {
			return err
		}
		count++
		return h.RecordValue(max(int64(l.LatencyUs), 1))
	})
	if err != nil {
		return nil, prov, err
	}
	for _, m := range members {
		_ = m.quit(60 * time.Second)
	}
	var producedStats map[string]any
	_ = json.Unmarshal(produceOut, &producedStats)
	ms := func(q float64) float64 { return float64(h.ValueAtQuantile(q)) / 1000 }
	drain := allDecided - prodDone
	sustained := ms(99) <= 1000 && drain <= 2*time.Second
	return map[string]any{
		"payments": n, "decisions": count, "produce": producedStats,
		"production_s": prodDone.Seconds(), "all_decided_s": allDecided.Seconds(), "drain_after_production_s": drain.Seconds(),
		"decided_per_s": float64(n) / allDecided.Seconds(),
		"latency_ms":    map[string]float64{"p50": ms(50), "p90": ms(90), "p99": ms(99), "p999": ms(99.9), "max": float64(h.Max()) / 1000},
		"max_lag":       maxLag, "lag_samples": samples,
		"sustained": sustained, "sustained_rule": "p99 latency <= 1 s and the backlog drains within 2 s of the last payment",
	}, prov, nil
}
