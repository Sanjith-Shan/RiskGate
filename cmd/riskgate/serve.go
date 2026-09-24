package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
)

type serveConfig struct {
	addr             string
	modelDir         string
	rulesPath        string
	listsPath        string
	tablePath        string
	state            string
	sharding         string
	shards           int
	snapshotDir      string
	snapshotInterval time.Duration
	decisionLog      string
	labelLog         string
	rulesHistory     string
	logContributions int
	webhookSecrets   string
	webhookTolerance time.Duration
	maxFutureSkew    time.Duration
	provenance       string
	logLevel         string
}

func serveFlags(fs *flag.FlagSet) *serveConfig {
	c := &serveConfig{}
	fs.StringVar(&c.addr, "addr", "127.0.0.1:8080", "listen address")
	fs.StringVar(&c.modelDir, "model", "", "model directory from python/train.py (empty: run without a model; risk_score is missing to rules)")
	fs.StringVar(&c.rulesPath, "rules", "rules/default.rules", "rule set to deploy when there is no snapshot")
	fs.StringVar(&c.listsPath, "lists", "rules/lists.json", "named lists for the rules (empty: none)")
	fs.StringVar(&c.tablePath, "table", "", "backtest feature table from `riskgate table` (empty: backtests disabled)")
	fs.StringVar(&c.state, "state", "exact", "velocity state: exact, bucketed or sketch")
	fs.StringVar(&c.sharding, "sharding", "sharded", "concurrency: sharded, locked (one mutex) or syncmap")
	fs.IntVar(&c.shards, "shards", 64, "shard count for -sharding sharded")
	fs.StringVar(&c.snapshotDir, "snapshot-dir", "var", "directory for the snapshot, and the default home of the logs and rule history (empty: no snapshots)")
	fs.DurationVar(&c.snapshotInterval, "snapshot-interval", time.Minute, "how often to snapshot under traffic (0: only at shutdown)")
	fs.StringVar(&c.decisionLog, "decision-log", "", "decision log JSONL (default <snapshot-dir>/decisions.jsonl; \"off\" disables)")
	fs.StringVar(&c.labelLog, "label-log", "", "label log JSONL (default <snapshot-dir>/labels.jsonl; \"off\" keeps labels in memory)")
	fs.StringVar(&c.rulesHistory, "rules-history", "", "directory of every deployed rule-set version (default <snapshot-dir>/rules-history)")
	fs.IntVar(&c.logContributions, "log-contributions", 10, "Saabas contributions logged per decision, largest first (0: all)")
	fs.StringVar(&c.webhookSecrets, "webhook-secrets", "", "comma-separated Clearinghouse signing secrets; prefer RISKGATE_WEBHOOK_SECRETS")
	fs.DurationVar(&c.webhookTolerance, "webhook-tolerance", 5*time.Minute, "accepted webhook signature timestamp skew")
	fs.DurationVar(&c.maxFutureSkew, "max-future-skew", service.DefaultMaxFutureSkew, "refuse a payment whose created is further ahead of the latest accepted one and the clock (0: no bound)")
	fs.StringVar(&c.provenance, "provenance", "", "data label shown on the page (default from the model: \"SYNTHETIC DATA\" or \"IEEE-CIS (local only)\")")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn or error")
	return c
}

// envOverrides applies RISKGATE_<FLAG> for every flag not set on the
// command line.
func envOverrides(fs *flag.FlagSet) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || err != nil {
			return
		}
		env := "RISKGATE_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(env); ok {
			if e := fs.Set(f.Name, v); e != nil {
				err = fmt.Errorf("%s: %w", env, e)
			}
		}
	})
	return err
}

// under resolves a path defaulting to a file in the snapshot directory.
func under(dir, path, name string) string {
	switch {
	case path == "off":
		return ""
	case path != "":
		return path
	case dir == "":
		return ""
	}
	return filepath.Join(dir, name)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := serveFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := envOverrides(fs); err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := buildConfig(c, logger)
	if err != nil {
		return err
	}
	svc, err := service.New(cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    c.addr,
		Handler: svc.Handler(),
		// Assess requests are small and fast; the generous write timeout is
		// for a backtest over the full table.
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	snapCtx, stopSnaps := context.WithCancel(context.Background())
	snapsDone := make(chan struct{})
	go func() {
		defer close(snapsDone)
		svc.RunSnapshots(snapCtx, c.snapshotInterval)
	}()

	errc := make(chan error, 1)
	go func() {
		logger.Info("riskgate listening", "addr", c.addr, "ruleset_version", svc.RulesetVersion(),
			"state", cfg.StateDesc, "model", c.modelDir != "", "backtests", cfg.Table != nil, "provenance", cfg.Provenance)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		stopSnaps()
		<-snapsDone
		return errors.Join(err, svc.Close())
	case <-ctx.Done():
	}

	// Graceful shutdown: stop taking requests and let in-flight ones finish,
	// then take the final snapshot with no traffic running, so it is exactly
	// the state the answered requests left behind.
	logger.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = srv.Shutdown(shutCtx)
	stopSnaps()
	<-snapsDone
	if cerr := svc.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if err == nil {
		logger.Info("stopped cleanly")
	}
	return err
}

func buildConfig(c *serveConfig, logger *slog.Logger) (service.Config, error) {
	cat := schema.Default()
	st, desc, err := service.NewState(c.state, c.sharding, c.shards)
	if err != nil {
		return service.Config{}, err
	}
	cfg := service.Config{
		Catalog:          cat,
		State:            st,
		StateDesc:        desc,
		DecisionLog:      under(c.snapshotDir, c.decisionLog, "decisions.jsonl"),
		LabelLog:         under(c.snapshotDir, c.labelLog, "labels.jsonl"),
		RulesHistoryDir:  under(c.snapshotDir, c.rulesHistory, "rules-history"),
		LogContributions: c.logContributions,
		WebhookTolerance: c.webhookTolerance,
		MaxFutureSkew:    c.maxFutureSkew,
		Provenance:       c.provenance,
		Logger:           logger,
	}
	if c.maxFutureSkew == 0 {
		cfg.MaxFutureSkew = -1 // the flag's 0 is "off"; the Config's is the default
	}
	if c.snapshotDir != "" {
		if err := os.MkdirAll(c.snapshotDir, 0o755); err != nil {
			return cfg, err
		}
		cfg.SnapshotPath = filepath.Join(c.snapshotDir, "riskgate.snap")
	}
	for _, s := range strings.Split(c.webhookSecrets, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.WebhookSecrets = append(cfg.WebhookSecrets, s)
		}
	}

	if c.modelDir != "" {
		if cfg.Scorer, err = model.LoadScorer(c.modelDir, cat); err != nil {
			return cfg, fmt.Errorf("model %s: %w", c.modelDir, err)
		}
	} else {
		logger.Warn("no -model: risk_score is missing to every rule and 0 in responses")
	}
	if cfg.Provenance == "" {
		cfg.Provenance = "IEEE-CIS (local only)"
		if cfg.Scorer == nil || cfg.Scorer.Synthetic() {
			cfg.Provenance = "SYNTHETIC DATA"
		}
	}

	if c.rulesPath != "" {
		b, err := os.ReadFile(c.rulesPath)
		if err != nil {
			return cfg, err
		}
		cfg.Rules = string(b)
	}
	if c.listsPath != "" {
		if cfg.ListsJSON, err = os.ReadFile(c.listsPath); err != nil {
			return cfg, err
		}
	}
	if c.tablePath != "" {
		start := time.Now()
		if cfg.Table, err = backtest.Load(c.tablePath, cat); err != nil {
			return cfg, err
		}
		logger.Info("loaded backtest table", "rows", cfg.Table.N, "took", time.Since(start).Round(time.Millisecond))
	}
	return cfg, nil
}
