// Package service is RiskGate's online service: the HTTP API Clearinghouse
// calls on every payment, the webhook receiver that turns disputes into
// labels, the rule-authoring API and page, and the machinery that makes
// every decision durable and replayable (decision log, rule history,
// snapshots).
//
// One request to POST /v1/assess runs the same code the offline export and
// the backtester run: features.Engine.ScoreAndUpdate computes the velocity
// features (scoring before updating, so a payment never sees itself), the
// model fills :risk_score:, and the live rules.RuleSet decides.
package service

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// Config is everything New needs. Zero values take the documented
// defaults; State and WebhookSecrets are required.
type Config struct {
	// Catalog defaults to schema.Default().
	Catalog *schema.Catalog
	// State holds the velocity features. It must be safe for concurrent
	// use (see NewState).
	State features.State
	// StateDesc describes State for snapshot mismatch errors, e.g.
	// "exact/sharded/64".
	StateDesc string
	// Scorer fills :risk_score:. Nil runs without a model: risk_score is
	// missing to the rules and 0 in responses.
	Scorer *model.Scorer

	// MaxFutureSkew bounds how far a payment's created may be ahead of the
	// later of the latest accepted created and the wall clock
	// (DefaultMaxFutureSkew); negative turns the bound off.
	MaxFutureSkew time.Duration

	// Rules and ListsJSON are the rule set to start with when there is no
	// snapshot to restore.
	Rules     string
	ListsJSON []byte
	// RulesHistoryDir keeps every deployed version on disk, for audit.
	RulesHistoryDir string

	// Table is the backtest feature table; nil disables backtests.
	Table *backtest.Table

	// DecisionLog is the JSONL decision log path; "" disables it.
	DecisionLog string
	// LogBuffer is the decision-log channel capacity (DefaultLogBuffer).
	LogBuffer int
	// LogContributions is how many Saabas contributions each decision logs,
	// largest magnitude first; <= 0 logs all of them.
	LogContributions int

	// LabelLog is the JSONL label log path; "" keeps labels in memory.
	LabelLog string

	// SnapshotPath is the snapshot file; "" disables snapshots.
	SnapshotPath string

	// WebhookSecrets are the accepted Clearinghouse signing secrets.
	WebhookSecrets []string
	// WebhookTolerance bounds signature timestamp skew
	// (webhook.DefaultTolerance).
	WebhookTolerance time.Duration
	// DedupeTTL is how long a processed webhook event id is remembered.
	DedupeTTL time.Duration

	// IdempotencyTTL and IdempotencyMaxEntries bound the idempotency store.
	IdempotencyTTL        time.Duration
	IdempotencyMaxEntries int

	// Provenance labels the data behind the model and backtests, shown on
	// the page: for example "SYNTHETIC DATA" or "IEEE-CIS (local only)".
	Provenance string

	// Now is the wall clock (time.Now). Event time comes from requests.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service is the running service. Create it with New, serve Handler, and
// call Close on shutdown.
type Service struct {
	cfg      Config
	cat      *schema.Catalog
	engine   *features.Engine
	scorer   *model.Scorer
	riskSlot int
	log      *slog.Logger
	now      func() time.Time

	rules    *ruleStore
	idem     *idemStore
	labels   *LabelStore
	dlog     *DecisionLog
	dedupe   *webhook.Deduper
	verifier *webhook.Verifier
	bt       *backtests
	metrics  metrics

	bufs   sync.Pool // *assessBuf
	bootID [8]byte   // hex; makes assessment ids unique across restarts
	seq    atomic.Uint64

	maxSkew       int64        // seconds; negative: no bound (see checkCreated)
	latestCreated atomic.Int64 // latest accepted created, Unix seconds

	snapMu sync.Mutex // one snapshot at a time
	// cut keeps a snapshot from saving a payment in the velocity state
	// without its idempotent answer. An assess with an Idempotency-Key holds
	// it shared from its velocity update until its answer is stored;
	// Snapshot holds it exclusively while it copies the idempotency store.
	cut          sync.RWMutex
	lastSnapshot atomic.Int64
	closed       atomic.Bool

	handler http.Handler
}

// New builds the service: it validates the webhook configuration (refusing
// to start without a usable secret, since forged disputes would become
// fraud labels), restores the snapshot if there is one, and otherwise
// deploys cfg.Rules as version 1.
func New(cfg Config) (*Service, error) {
	if cfg.Catalog == nil {
		cfg.Catalog = schema.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.State == nil {
		return nil, errors.New("service: no velocity state configured")
	}
	verifier := webhook.NewVerifier(cfg.WebhookSecrets...)
	verifier.Tolerance = cfg.WebhookTolerance
	if err := verifier.Validate(); err != nil {
		return nil, fmt.Errorf("service: webhook secrets: %w (set RISKGATE_WEBHOOK_SECRETS)", err)
	}
	engine, err := features.NewEngine(cfg.Catalog, cfg.State)
	if err != nil {
		return nil, err
	}
	risk, ok := cfg.Catalog.Lookup(schema.RiskScoreField.Name)
	if !ok || risk.Kind != schema.Number {
		return nil, errors.New("service: catalog has no numeric risk_score")
	}
	labels, err := OpenLabelStore(cfg.LabelLog)
	if err != nil {
		return nil, err
	}
	s := &Service{
		cfg: cfg, cat: cfg.Catalog, engine: engine, scorer: cfg.Scorer, riskSlot: risk.Slot,
		log: cfg.Logger, now: cfg.Now,
		rules:    newRuleStore(cfg.Catalog, cfg.RulesHistoryDir, cfg.Now),
		idem:     newIdemStore(cfg.IdempotencyTTL, cfg.IdempotencyMaxEntries, cfg.Now),
		labels:   labels,
		dedupe:   webhook.NewDeduper(webhook.DeduperConfig{TTL: cfg.DedupeTTL, Now: cfg.Now}),
		verifier: verifier,
		bt:       newBacktests(cfg.Table),
		maxSkew:  -1,
	}
	if cfg.MaxFutureSkew >= 0 {
		s.maxSkew = int64(cmp.Or(cfg.MaxFutureSkew, DefaultMaxFutureSkew) / time.Second)
	}
	if _, err := rand.Read(s.bootID[:4]); err != nil {
		return nil, err
	}
	hex.Encode(s.bootID[:], s.bootID[:4])
	nIn := 0
	if s.scorer != nil {
		nIn = s.scorer.NumFeatures()
	}
	s.bufs.New = func() any { return newAssessBuf(s.cat, nIn) }

	fail := func(err error) (*Service, error) {
		_ = labels.Close()
		return nil, err
	}
	if err := s.rules.loadHistory(); err != nil {
		return fail(fmt.Errorf("service: rule history: %w", err))
	}
	restored, err := s.restoreSnapshot()
	if err != nil {
		return fail(err)
	}
	if !restored {
		if _, err := s.rules.deploy(cfg.Rules, cfg.ListsJSON); err != nil {
			return fail(fmt.Errorf("service: initial rules: %w", err))
		}
	}
	torn, err := labels.replayLog()
	if err != nil {
		return fail(fmt.Errorf("service: label log: %w", err))
	}
	if torn > 0 {
		s.log.Warn("label log: cut off a torn final line (an unacknowledged label, which Clearinghouse redelivers)", "path", cfg.LabelLog, "bytes", torn)
	}
	if cfg.DecisionLog != "" {
		var names []string
		var sum string
		if s.scorer != nil {
			names, sum = s.scorer.FeatureNames(), s.scorer.SHA256()
		}
		if s.dlog, err = NewDecisionLog(cfg.DecisionLog, s.cat, names, sum, cfg.LogContributions, cfg.LogBuffer, 0); err != nil {
			return fail(err)
		}
	}
	s.handler = s.routes()
	return s, nil
}

// Handler returns the service's HTTP handler.
func (s *Service) Handler() http.Handler { return s.handler }

// RulesetVersion returns the live rule-set version.
func (s *Service) RulesetVersion() uint64 { return s.rules.current().Version }

// Close takes a final snapshot and closes the logs. Call it after the HTTP
// server has stopped accepting requests, so the snapshot is a single
// point in time: the state every answered request left behind. Requests
// that outlive the shutdown's drain timeout are still safe: their decision
// log records are dropped and counted, and their labels refused with 503.
func (s *Service) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	var errs []error
	if s.cfg.SnapshotPath != "" {
		errs = append(errs, s.Snapshot())
	}
	errs = append(errs, s.dlog.Close(), s.labels.Close())
	return errors.Join(errs...)
}

// routes wires every endpoint. Method-qualified patterns (Go 1.22) make the
// mux answer 405 with an Allow header for the wrong method.
func (s *Service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/assess", s.handleAssess)
	mux.Handle("POST /v1/webhooks/clearinghouse", s.counted(routeWebhook, s.webhookHandler()))
	mux.Handle("GET /v1/rules", s.counted(routeRulesGet, http.HandlerFunc(s.handleRulesGet)))
	mux.Handle("PUT /v1/rules", s.counted(routeRulesPut, http.HandlerFunc(s.handleRulesPut)))
	mux.Handle("POST /v1/rules/test", s.counted(routeRulesTest, http.HandlerFunc(s.handleRulesTest)))
	mux.Handle("GET /v1/rules/sweep", s.counted(routeRulesSweep, http.HandlerFunc(s.handleSweep)))
	mux.Handle("GET /v1/rules/shadow", s.counted(routeRulesShadow, http.HandlerFunc(s.handleShadow)))
	mux.Handle("GET /v1/info", s.counted(routeInfo, http.HandlerFunc(s.handleInfo)))
	mux.Handle("GET /healthz", s.counted(routeHealth, http.HandlerFunc(s.handleHealth)))
	mux.Handle("GET /metrics", s.counted(routeMetrics, http.HandlerFunc(s.handleMetrics)))
	mux.Handle("GET /{$}", s.counted(routePage, http.HandlerFunc(s.handlePage)))
	return mux
}

// statusWriter records the status code for the request counter.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// counted counts requests by route and status. The assess handler counts
// its own, to keep this wrapper's allocation off the payment path.
func (s *Service) counted(route int, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		s.metrics.count(route, sw.code)
	})
}

// webhookHandler receives Clearinghouse events: signature verified, deduped
// by event id, and recorded as labels before the 200 goes out.
func (s *Service) webhookHandler() http.Handler {
	h := webhook.NewHandler(s.verifier, s.dedupe, func(_ context.Context, e *webhook.Event) error {
		return s.labels.Record(e)
	})
	h.Logger = s.log
	return h
}
