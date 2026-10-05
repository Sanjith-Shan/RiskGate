package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// riskgate stream: the Kafka pipeline (internal/stream).
//
//	riskgate stream topics    create (or -delete) the pipeline's topics
//	riskgate stream run       run one or more stages: route, aggregate, join, labels
//	riskgate stream produce   replay the dataset into the payments topic, in event-time order
//	riskgate stream decisions read the decisions topic back, deduplicated, in event-time order
//	riskgate stream backtest  backtest a rule over the decisions read back, labelled by the disputes topic
func streamCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: riskgate stream topics|run|produce|decisions|backtest [flags]")
	}
	switch args[0] {
	case "topics":
		return streamTopics(args[1:])
	case "run":
		return streamRun(args[1:])
	case "produce":
		return streamProduce(args[1:])
	case "decisions":
		return streamDecisions(args[1:])
	case "backtest":
		return streamBacktest(args[1:])
	}
	return fmt.Errorf("riskgate stream: unknown command %q (want topics, run, produce, decisions or backtest)", args[0])
}

type kafkaFlags struct {
	brokers string
	prefix  string
}

func (k *kafkaFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&k.brokers, "brokers", "127.0.0.1:19092", "comma-separated Kafka bootstrap brokers")
	fs.StringVar(&k.prefix, "prefix", "riskgate", "topic name prefix: <prefix>.payments, <prefix>.decisions, ...")
}

func (k *kafkaFlags) list() []string        { return strings.Split(k.brokers, ",") }
func (k *kafkaFlags) topics() stream.Topics { return stream.TopicsWithPrefix(k.prefix) }

func (k *kafkaFlags) client(extra ...kgo.Opt) (*kgo.Client, error) {
	return kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(k.list()...)}, extra...)...)
}

func streamTopics(args []string) error {
	fs := flag.NewFlagSet("stream topics", flag.ExitOnError)
	var k kafkaFlags
	k.register(fs)
	l := stream.DefaultLayout
	var p, e, j, d int
	fs.IntVar(&p, "payments", int(l.Payments), "payments partitions")
	fs.IntVar(&e, "entity", int(l.EntityEvents), "entity-events partitions (velocity state shards)")
	fs.IntVar(&j, "parts", int(l.Parts), "parts partitions (joiners)")
	fs.IntVar(&d, "decisions", int(l.Decisions), "decisions partitions")
	del := fs.Bool("delete", false, "delete the topics instead")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cl, err := k.client()
	if err != nil {
		return err
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if *del {
		return stream.DeleteTopics(ctx, cl, k.topics())
	}
	return stream.CreateTopics(ctx, cl, k.topics(), stream.Layout{Payments: int32(p), EntityEvents: int32(e), Parts: int32(j), Decisions: int32(d), Disputes: 1})
}

func streamRun(args []string) error {
	fs := flag.NewFlagSet("stream run", flag.ExitOnError)
	var k kafkaFlags
	k.register(fs)
	stages := fs.String("stages", "route,aggregate,join,labels", "stages this process runs")
	modelDir := fs.String("model", "", "model directory (empty: risk_score is missing to the rules)")
	rulesPath := fs.String("rules", "rules/default.rules", "rule set the joiner applies")
	listsPath := fs.String("lists", "rules/lists.json", "named lists for the rules")
	version := fs.Uint64("ruleset-version", 1, "rule-set version recorded on every decision")
	history := fs.String("rules-history", "var/stream/rules-history", "directory the rule set is recorded in, for `riskgate audit`")
	stateDir := fs.String("state-dir", "var/stream/state", "snapshot store shared by every process of the pipeline")
	labelLog := fs.String("label-log", "var/stream/labels.jsonl", "label log of the labels stage")
	instance := fs.String("instance-id", "", "static group membership id (a restart within -session-timeout keeps its partitions)")
	session := fs.Duration("session-timeout", 10*time.Second, "group session timeout")
	every := fs.Duration("checkpoint-every", 5*time.Second, "how often stateful stages snapshot and commit")
	maxPoll := fs.Int("max-poll", 4096, "records per poll")
	contrib := fs.Int("log-contributions", 10, "Saabas contributions per decision line (0: all)")
	arrival := fs.Bool("arrival-order", false, "negative control: aggregators apply events in arrival order, with no watermarks")
	metricsAddr := fs.String("metrics-addr", "", "serve stage metrics as JSON at http://<addr>/metrics (empty: off)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := envOverrides(fs); err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("pid", os.Getpid(), "instance", *instance)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cat := schema.Default()
	metrics := &stream.Metrics{}
	base := stream.Config{
		Brokers: k.list(), Topics: k.topics(), InstanceID: *instance, SessionTimeout: *session,
		Store: stream.DirStore{Dir: *stateDir}, CheckpointEvery: *every, MaxPollRecords: *maxPoll,
		Catalog: cat, ArrivalOrder: *arrival, Logger: logger, Metrics: metrics,
	}
	names := strings.Split(*stages, ",")
	for _, s := range names {
		switch strings.TrimSpace(s) {
		case stream.StageJoin:
			var scorer *model.Scorer
			var err error
			if *modelDir != "" {
				if scorer, err = model.LoadScorer(*modelDir, cat); err != nil {
					return err
				}
			}
			text, err := os.ReadFile(*rulesPath)
			if err != nil {
				return err
			}
			var lists []byte
			if *listsPath != "" {
				if lists, err = os.ReadFile(*listsPath); err != nil {
					return err
				}
			}
			rs, err := service.CompileRules(cat, string(text), lists, *version, *history)
			if err != nil {
				return err
			}
			if base.Decider, err = stream.NewDecider(cat, scorer, rs, *contrib); err != nil {
				return err
			}
		case stream.StageLabels:
			store, torn, err := service.OpenLabelLog(*labelLog)
			if err != nil {
				return err
			}
			if torn > 0 {
				logger.Warn("label log: cut off a torn final line", "bytes", torn)
			}
			defer store.Close()
			base.Labels = store
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, len(names))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// GET /metrics: stage metrics as JSON. POST /quit: stop cleanly, which
	// is how the experiment driver asks for a graceful stop on Windows,
	// where a process cannot be sent SIGTERM.
	if *metricsAddr != "" {
		ln, err := net.Listen("tcp", *metricsAddr)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metrics.Snapshot())
		})
		mux.HandleFunc("POST /quit", func(w http.ResponseWriter, _ *http.Request) {
			cancel()
			w.WriteHeader(http.StatusAccepted)
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
	}

	for i, s := range names {
		cfg := base
		cfg.Stage = strings.TrimSpace(s)
		r, err := stream.NewRunner(runCtx, cfg)
		if err != nil {
			cancel()
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if errs[i] = r.Run(runCtx); errs[i] != nil {
				logger.Error("stage failed", "stage", cfg.Stage, "error", errs[i])
				cancel() // one stage failing stops the process; a supervisor restarts it
			}
		}()
	}
	logger.Info("stream pipeline running", "stages", *stages, "brokers", k.brokers, "prefix", k.prefix)
	wg.Wait()
	m, _ := json.Marshal(metrics.Snapshot())
	logger.Info("stopped", "metrics", json.RawMessage(m))
	return errors.Join(errs...)
}

func streamProduce(args []string) error {
	fs := flag.NewFlagSet("stream produce", flag.ExitOnError)
	var k kafkaFlags
	k.register(fs)
	dataDir := fs.String("data", "data", "data directory (raw/ and cache/, or synth/ with -synthetic)")
	synthetic := fs.Bool("synthetic", false, "replay the SYNTHETIC dataset")
	limit := fs.Int("limit", 0, "replay only the first N payments (0: all)")
	rate := fs.Float64("rate", 0, "payments per second, open loop (0: as fast as Kafka takes them)")
	beat := fs.Int("heartbeat-every", 1000, "heartbeat every payments partition after this many payments")
	final := fs.Bool("final", true, "send the end-of-stream heartbeat when done")
	disputes := fs.Bool("disputes", true, "publish SIMULATED dispute events for fraudulent payments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := data.RealPaths(*dataDir)
	if *synthetic {
		paths = data.SyntheticPaths(*dataDir)
	}
	ds, err := data.Load(paths)
	if err != nil {
		return err
	}
	txns := ds.Txns
	if *limit > 0 && *limit < len(txns) {
		txns = txns[:*limit]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cl, err := k.client(kgo.RecordPartitioner(kgo.ManualPartitioner()), kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(5*time.Millisecond), kgo.MaxBufferedRecords(1<<20))
	if err != nil {
		return err
	}
	defer cl.Close()
	counts, err := stream.ReadPartitionCounts(ctx, cl, k.topics())
	if err != nil {
		return err
	}
	var progress atomic.Int64
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "produced %d of %d\n", progress.Load(), len(txns))
			}
		}
	}()
	st, err := stream.Produce(ctx, cl, txns, counts.Payments, stream.ProduceConfig{
		Topics: k.topics(), Rate: *rate, HeartbeatEvery: *beat, Final: *final, Disputes: *disputes, DisputePartitions: 1, Progress: &progress,
	})
	close(done)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{
		"payments": st.Payments, "heartbeats": st.Heartbeats, "disputes": st.Disputes,
		"took_ms": st.Took.Milliseconds(), "max_behind_ms": st.MaxBehind.Milliseconds(), "synthetic": ds.Synthetic,
	})
	fmt.Println(string(b))
	return nil
}

func streamDecisions(args []string) error {
	fs := flag.NewFlagSet("stream decisions", flag.ExitOnError)
	var k kafkaFlags
	k.register(fs)
	out := fs.String("out", "", "write one decision per payment, in event-time order, as a decision log (row-level: keep it under data/ or var/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	spill := "."
	if *out != "" {
		spill = filepath.Dir(*out)
	}
	ds, err := stream.ReadDecisions(ctx, k.list(), k.topics().Decisions, spill)
	if err != nil {
		return err
	}
	defer ds.Close()
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		if err := ds.WriteSorted(f); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(map[string]any{
		"records": ds.Records, "payments": ds.Len(), "duplicates": ds.Duplicates, "conflicting_duplicates": ds.Conflicts,
	})
	fmt.Println(string(b))
	if ds.Conflicts > 0 {
		return fmt.Errorf("%d re-sent decisions differ from the first (e.g. %s)", ds.Conflicts, ds.Example)
	}
	return nil
}

func streamBacktest(args []string) error {
	fs := flag.NewFlagSet("stream backtest", flag.ExitOnError)
	var k kafkaFlags
	k.register(fs)
	logPath := fs.String("log", "", "decision log from `riskgate stream decisions -out` (required)")
	ruleText := fs.String("rule", "", "the proposed rule (required)")
	rulesPath := fs.String("rules", "rules/default.rules", "the rule set in force")
	listsPath := fs.String("lists", "rules/lists.json", "named lists")
	asOf := fs.Bool("as-of-end", true, "count only the disputes that had arrived by the last payment (UseLabelTime)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath == "" || *ruleText == "" {
		return errors.New("stream backtest: -log and -rule are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cat := schema.Default()
	lists, err := os.ReadFile(*listsPath)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(*rulesPath)
	if err != nil {
		return err
	}
	current, err := service.CompileRules(cat, string(cur), lists, 1, "")
	if err != nil {
		return err
	}
	proposed, err := service.CompileRules(cat, *ruleText, lists, 1, "")
	if err != nil {
		return err
	}
	if len(proposed.Rules) != 1 {
		return fmt.Errorf("stream backtest: -rule holds %d rules, want 1", len(proposed.Rules))
	}
	disputes, err := stream.ReadDisputes(ctx, k.list(), k.topics().Disputes)
	if err != nil {
		return err
	}
	f, err := os.Open(*logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	t, err := stream.TableFromLog(f, cat, stream.DisputeLabels(disputes))
	if err != nil {
		return err
	}
	opt := backtest.Options{}
	if *asOf {
		_, hi := t.TimeSpan()
		opt.AsOf, opt.UseLabelTime = hi, true
	}
	rep, err := backtest.Backtest(t, current, proposed.Rules[0], opt)
	if err != nil {
		return err
	}
	fmt.Println(rep.SummaryText)
	return nil
}
