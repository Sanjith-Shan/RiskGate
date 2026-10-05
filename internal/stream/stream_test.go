package stream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/data/synth"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
)

const testRules = `allow  if :purchaser_email_domain: in @trusted and :risk_score: < 5
block  if :card_txn_count_1h: >= 6
block  if :risk_score: >= 60
review if :purchaser_email_domain: = "anonymous.com" and :amount: > 100
shadow block  if :distinct_cards_per_device_24h: >= 22
shadow review if :amount: > 150
`

const testLists = `{"trusted": ["yahoo.com"]}`

var testLayout = Layout{Payments: 4, EntityEvents: 3, Parts: 3, Decisions: 2, Disputes: 1}

// pipeline is a fake cluster with the pipeline's topics and a decider.
type pipeline struct {
	t       *testing.T
	brokers []string
	topics  Topics
	store   Store
	decider *Decider
	txns    []data.Txn
	cat     *schema.Catalog
	scorer  *model.Scorer
	rules   *rules.RuleSet
	metrics *Metrics
}

func newPipeline(t *testing.T, rows int) *pipeline {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.GroupMinSessionTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p := &pipeline{t: t, brokers: c.ListenAddrs(), topics: TopicsWithPrefix("rgtest"), store: DirStore{Dir: t.TempDir()}, cat: schema.Default(), metrics: &Metrics{}}
	cl := p.client()
	if err := CreateTopics(context.Background(), cl, p.topics, testLayout); err != nil {
		t.Fatal(err)
	}
	cl.Close()
	if p.scorer, err = model.LoadScorer(filepath.Join("..", "service", "testdata", "model"), p.cat); err != nil {
		t.Fatal(err)
	}
	lists, err := rules.ParseLists([]byte(testLists))
	if err != nil {
		t.Fatal(err)
	}
	if p.rules, err = rules.Load(testRules, rules.Env{Catalog: p.cat, Lists: lists}, 1); err != nil {
		t.Fatal(err)
	}
	if p.decider, err = NewDecider(p.cat, p.scorer, p.rules, 0); err != nil {
		t.Fatal(err)
	}
	p.txns, _ = synth.Generate(synth.Config{Rows: rows, Days: 6, Customers: rows / 8, Seed: 7})
	return p
}

func (p *pipeline) client() *kgo.Client {
	cl, err := kgo.NewClient(kgo.SeedBrokers(p.brokers...), kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		p.t.Fatal(err)
	}
	return cl
}

func (p *pipeline) produce(txns []data.Txn, final bool) {
	p.t.Helper()
	cl := p.client()
	defer cl.Close()
	if _, err := Produce(context.Background(), cl, txns, testLayout.Payments, ProduceConfig{Topics: p.topics, HeartbeatEvery: 97, Final: final}); err != nil {
		p.t.Fatal(err)
	}
}

// stage is a running Runner.
type stage struct {
	r      *Runner
	cancel context.CancelFunc
	done   chan error
}

func (p *pipeline) start(name, instance string, arrival bool) *stage {
	p.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, err := NewRunner(ctx, Config{
		Brokers: p.brokers, Topics: p.topics, Stage: name, InstanceID: instance, SessionTimeout: 2 * time.Second,
		Store: p.store, CheckpointEvery: 30 * time.Millisecond, MaxPollRecords: 64, Decider: p.decider,
		ArrivalOrder: arrival, Metrics: p.metrics, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		cancel()
		p.t.Fatal(err)
	}
	s := &stage{r: r, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- r.Run(ctx) }()
	return s
}

func (s *stage) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	if err := <-s.done; err != nil {
		t.Fatalf("stage stopped with %v", err)
	}
}

func (s *stage) crash(t *testing.T) {
	t.Helper()
	s.r.Crash()
	if err := <-s.done; err != nil && !errors.Is(err, errCrashed) {
		t.Fatalf("crashed stage returned %v", err)
	}
}

// waitDecisions waits until every payment has a decision.
func (p *pipeline) waitDecisions(n int) *DecisionSet {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for {
		ds, err := ReadDecisions(ctx, p.brokers, p.topics.Decisions)
		if err != nil {
			p.t.Fatal(err)
		}
		if len(ds.Lines) >= n {
			return ds
		}
		select {
		case <-ctx.Done():
			p.t.Fatalf("only %d of %d payments decided; metrics %+v", len(ds.Lines), n, p.metrics.Snapshot())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// check compares every streamed decision with the offline pipeline: the
// feature row bit for bit against features.Replay, the score against the
// Scorer, the decision and rule against the rule set.
func (p *pipeline) check(ds *DecisionSet) (mismatched int) {
	p.t.Helper()
	if ds.Conflicts != 0 {
		p.t.Fatalf("%d re-sent decisions differ from the first (e.g. %s)", ds.Conflicts, ds.Example)
	}
	engine, err := features.NewEngine(p.cat, features.NewExact(0))
	if err != nil {
		p.t.Fatal(err)
	}
	riskSlot := p.cat.MustLookup(schema.RiskScoreField.Name).Slot
	err = features.Replay(engine, p.txns, func(tx *data.Txn, off schema.Row) error {
		id := data.PaymentIDPrefix + strconv.FormatInt(tx.ID, 10)
		line, ok := ds.Lines[id]
		if !ok {
			p.t.Fatalf("no decision for %s", id)
		}
		var e service.LogEntry
		if err := jsonUnmarshal(line, &e); err != nil {
			p.t.Fatal(err)
		}
		row, err := e.Row(p.cat)
		if err != nil {
			p.t.Fatal(err)
		}
		risk, _, raw := p.scorer.Score(off)
		off.Num[riskSlot] = float64(risk)
		want := p.rules.Evaluate(off)
		bad := !rowsEqual(row, off) || e.RawScore == nil || math.Float64bits(*e.RawScore) != math.Float64bits(raw) ||
			e.RiskScore != risk || e.Decision != want.Action.String()
		if want.Rule != nil && (e.RuleID == nil || *e.RuleID != want.Rule.ID) {
			bad = true
		}
		if bad {
			mismatched++
		}
		return nil
	})
	if err != nil {
		p.t.Fatal(err)
	}
	if len(ds.Lines) != len(p.txns) {
		p.t.Fatalf("%d decisions for %d payments", len(ds.Lines), len(p.txns))
	}
	return mismatched
}

func rowsEqual(a, b schema.Row) bool {
	for i := range a.Num {
		x, y := a.Num[i], b.Num[i]
		if math.Float64bits(x) != math.Float64bits(y) && !(math.IsNaN(x) && math.IsNaN(y)) {
			return false
		}
	}
	for i := range a.Str {
		if a.Str[i] != b.Str[i] {
			return false
		}
	}
	return true
}

func startAll(p *pipeline, arrival bool) []*stage {
	return []*stage{p.start(StageRoute, "", false), p.start(StageAggregate, "", arrival), p.start(StageJoin, "", false)}
}

func stopAll(t *testing.T, ss []*stage) {
	var wg sync.WaitGroup
	for _, s := range ss {
		wg.Add(1)
		go func() { defer wg.Done(); s.stop(t) }()
	}
	wg.Wait()
}

// TestStreamParity: every payment through Kafka gets exactly the features,
// score and decision the offline pipeline computes.
func TestStreamParity(t *testing.T) {
	p := newPipeline(t, 4000)
	ss := startAll(p, false)
	p.produce(p.txns, true)
	ds := p.waitDecisions(len(p.txns))
	stopAll(t, ss)
	if n := p.check(ds); n != 0 {
		t.Fatalf("%d of %d decisions differ from the offline pipeline", n, len(p.txns))
	}
	m := p.metrics.Snapshot()
	if m[StageAggregate].Late != 0 || m[StageRoute].OrderViolations != 0 {
		t.Fatalf("late %d, order violations %d", m[StageAggregate].Late, m[StageRoute].OrderViolations)
	}
}

// TestStreamArrivalOrderBreaksParity is the merger's negative control: with
// several payments partitions and no watermarks, shared keys see events in
// arrival order and some features differ. If this ever passes with zero
// mismatches, the parity test above is not testing the merger.
func TestStreamArrivalOrderBreaksParity(t *testing.T) {
	p := newPipeline(t, 4000)
	ss := startAll(p, true)
	p.produce(p.txns, true)
	ds := p.waitDecisions(len(p.txns))
	stopAll(t, ss)
	n := p.check(ds)
	t.Logf("arrival order: %d of %d decisions differ", n, len(p.txns))
	if n == 0 {
		t.Fatal("arrival order produced no mismatches; the control cannot detect a missing merger")
	}
}

// TestStreamCrashRecovery crashes the stateful stages repeatedly mid-stream
// (no checkpoint, no commit) and restarts them from their snapshots. Nothing
// may be lost or counted twice: every feature must still match.
func TestStreamCrashRecovery(t *testing.T) {
	p := newPipeline(t, 4000)
	route := p.start(StageRoute, "", false)
	agg := p.start(StageAggregate, "", false)
	join := p.start(StageJoin, "", false)

	chunks := 8
	for c := range chunks {
		lo, hi := c*len(p.txns)/chunks, (c+1)*len(p.txns)/chunks
		p.produce(p.txns[lo:hi], c == chunks-1)
		time.Sleep(150 * time.Millisecond)
		switch c % 3 {
		case 0:
			agg.crash(t)
			agg = p.start(StageAggregate, "", false)
		case 1:
			join.crash(t)
			join = p.start(StageJoin, "", false)
		case 2:
			route.crash(t)
			route = p.start(StageRoute, "", false)
		}
	}
	ds := p.waitDecisions(len(p.txns))
	stopAll(t, []*stage{route, agg, join})
	if n := p.check(ds); n != 0 {
		t.Fatalf("after crashes, %d of %d decisions differ", n, len(p.txns))
	}
	m := p.metrics.Snapshot()
	t.Logf("records re-delivered and dropped: aggregate %d, join %d; decisions re-sent %d", m[StageAggregate].Duplicates, m[StageJoin].Duplicates, ds.Duplicates)
}

// TestStreamRebalance scales the stateful stages out and back in while the
// stream runs, so partitions move between members with their state.
func TestStreamRebalance(t *testing.T) {
	p := newPipeline(t, 4000)
	route := p.start(StageRoute, "", false)
	agg1 := p.start(StageAggregate, "", false)
	join1 := p.start(StageJoin, "", false)
	third := len(p.txns) / 3
	p.produce(p.txns[:third], false)
	time.Sleep(200 * time.Millisecond)
	agg2 := p.start(StageAggregate, "", false)
	join2 := p.start(StageJoin, "", false)
	p.produce(p.txns[third:2*third], false)
	time.Sleep(200 * time.Millisecond)
	agg1.stop(t)
	join2.stop(t)
	p.produce(p.txns[2*third:], true)
	ds := p.waitDecisions(len(p.txns))
	stopAll(t, []*stage{route, agg2, join1})
	if n := p.check(ds); n != 0 {
		t.Fatalf("across rebalances, %d of %d decisions differ", n, len(p.txns))
	}
	m := p.metrics.Snapshot()
	if m[StageAggregate].Restores == 0 {
		t.Fatal("no aggregator partition was restored from a handed-over snapshot")
	}
}
