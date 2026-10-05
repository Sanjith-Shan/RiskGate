package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// verification is everything checked after a run: the decisions topic read
// back, the offline comparison, the audit, and the snapshot accounting.
type verification struct {
	Decisions struct {
		Records    int `json:"records"`
		Payments   int `json:"payments"`
		Duplicates int `json:"duplicates"`
		Conflicts  int `json:"conflicting_duplicates"`
	} `json:"decisions_topic"`
	Expected       int                  `json:"payments_expected"`
	Lost           int                  `json:"payments_without_decision"`
	Offline        json.RawMessage      `json:"offline_comparison"` // cmd/serveparity compare's report
	OfflineOK      bool                 `json:"offline_ok"`
	FeatureDiffer  int                  `json:"feature_rows_differ"`
	ScoreDiffer    int                  `json:"raw_score_rows_differ"`
	Audit          json.RawMessage      `json:"audit,omitempty"`
	State          *stream.StateSummary `json:"state,omitempty"`
	EntityExpected uint64               `json:"entity_events_expected,omitempty"`
	TookMs         int64                `json:"verify_ms"`
}

// verify reads the decisions back and checks them against the offline
// pipeline. export is a cmd/export directory built from the same data.
func (e *env) verify(ctx context.Context, expected int, export string, txns []data.Txn) (*verification, error) {
	start := time.Now()
	v := &verification{Expected: expected}
	logPath := filepath.Join(e.work, "stream_decisions.jsonl")
	out, err := e.run(ctx, "riskgate", append([]string{"stream", "decisions", "-out", logPath}, e.kafkaArgs()...)...)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(out, &v.Decisions); err != nil {
		return nil, fmt.Errorf("stream decisions output %q: %w", out, err)
	}
	v.Lost = expected - v.Decisions.Payments

	cmpPath := filepath.Join(e.work, "offline_compare.json")
	_, cerr := e.run(ctx, "serveparity", "compare", "-log", logPath, "-data", e.data, "-export", export, "-model", e.model, "-json", cmpPath, "-examples", "5")
	if b, err := os.ReadFile(cmpPath); err == nil {
		v.Offline = b
		var rep struct {
			OK  bool `json:"ok"`
			All struct {
				Feature int `json:"feature_rows_differ"`
				Raw     int `json:"raw_score_differ"`
			} `json:"all"`
		}
		_ = json.Unmarshal(b, &rep)
		v.OfflineOK, v.FeatureDiffer, v.ScoreDiffer = rep.OK, rep.All.Feature, rep.All.Raw
	} else if cerr != nil {
		return nil, cerr
	}

	if out, err := e.run(ctx, "riskgate", "audit", "-log", logPath, "-rules-history", filepath.Join(e.work, "rules-history"), "-model", e.model, "-json"); err == nil || len(out) > 0 {
		var a map[string]any
		if json.Unmarshal(out, &a) == nil {
			delete(a, "examples") // examples name payments; aggregates only
			v.Audit, _ = json.Marshal(a)
		}
	}

	st, err := stream.SummarizeSnapshots(stream.DirStore{Dir: filepath.Join(e.work, "state")}, stream.TopicsWithPrefix(e.prefix), e.layout.EntityEvents, e.layout.Parts)
	if err == nil {
		v.State = &st
	}
	for i := range txns {
		_, ok := features.KeysOf(&txns[i])
		for _, k := range ok {
			if k {
				v.EntityExpected++
			}
		}
	}
	v.TookMs = time.Since(start).Milliseconds()
	return v, nil
}

// logDiff compares two decision logs line by line, both in event-time
// order: decision, rule, scores to the bit, and every feature.
type logDiff struct {
	Compared       int `json:"compared"`
	OrderMismatch  int `json:"payment_order_mismatches"`
	DecisionDiffer int `json:"decision_or_rule_differ"`
	ScoreDiffer    int `json:"score_differ"`
	FeatureDiffer  int `json:"feature_rows_differ"`
	ExtraA, ExtraB int
}

func compareLogs(a, b string) (logDiff, error) {
	var d logDiff
	fa, err := os.Open(a)
	if err != nil {
		return d, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return d, err
	}
	defer fb.Close()
	ra, rb := bufio.NewReaderSize(fa, 1<<20), bufio.NewReaderSize(fb, 1<<20)
	next := func(r *bufio.Reader) (*service.LogEntry, error) {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return nil, err
		}
		var e service.LogEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		return &e, nil
	}
	same := func(x, y *float64) bool {
		if x == nil || y == nil {
			return x == y
		}
		return math.Float64bits(*x) == math.Float64bits(*y)
	}
	for {
		x, errA := next(ra)
		y, errB := next(rb)
		if errA == io.EOF || errB == io.EOF {
			for errA == nil {
				d.ExtraA++
				_, errA = next(ra)
			}
			for errB == nil {
				d.ExtraB++
				_, errB = next(rb)
			}
			return d, nil
		}
		if errA != nil {
			return d, errA
		}
		if errB != nil {
			return d, errB
		}
		d.Compared++
		if x.PaymentID != y.PaymentID {
			d.OrderMismatch++
			continue
		}
		rx, ry := "", ""
		if x.RuleID != nil {
			rx = *x.RuleID
		}
		if y.RuleID != nil {
			ry = *y.RuleID
		}
		if x.Decision != y.Decision || rx != ry || x.RulesetVersion != y.RulesetVersion {
			d.DecisionDiffer++
		}
		if x.RiskScore != y.RiskScore || !same(x.RawScore, y.RawScore) || !same(x.Probability, y.Probability) {
			d.ScoreDiffer++
		}
		fx, _ := json.Marshal(x.Features)
		fy, _ := json.Marshal(y.Features)
		if string(fx) != string(fy) {
			d.FeatureDiffer++
		}
	}
}

// backtestCheck is spec item 3's claim, checked: a backtest replayed from
// the decisions topic gives the same report as one over the offline table.
// The offline table is built the way `riskgate table` builds it (Replay,
// then the Scorer, labels from the dataset). The topic table is built from
// the decision log the topic was read into, given the same labels, so any
// difference is in the features or scores. Every rule of the rule files is
// backtested against the default rule set on both, and the reports compared
// whole. Then the topic table is labelled from the disputes topic instead:
// what a backtest that knows only the disputes that have arrived reports.
type backtestResult struct {
	Rows                int      `json:"rows"`
	ColumnsIdentical    bool     `json:"feature_and_score_columns_identical"`
	CellsDiffer         int      `json:"cells_differ"`
	RulesCompared       int      `json:"rules_compared"`
	ReportsIdentical    int      `json:"reports_identical"`
	ReportsDiffer       []string `json:"reports_differ,omitempty"`
	DisputesInTopic     int      `json:"fraud_disputes_in_topic"`
	DatasetFraud        int      `json:"fraud_in_dataset"`
	TopicLabelSummary   string   `json:"topic_labels_block_rule_summary,omitempty"`
	DatasetLabelSummary string   `json:"dataset_labels_block_rule_summary,omitempty"`
}

func (e *env) backtestCheck(ctx context.Context, txns []data.Txn, ruleFiles []string) (*backtestResult, error) {
	cat := schema.Default()
	scorer, err := model.LoadScorer(e.model, cat)
	if err != nil {
		return nil, err
	}
	engine, err := features.NewEngine(cat, features.NewExact(0))
	if err != nil {
		return nil, err
	}
	risk := cat.MustLookup(schema.RiskScoreField.Name).Slot
	b := backtest.NewBuilder(cat, len(txns))
	fraudByID := make(map[int64]int8, len(txns))
	res := &backtestResult{}
	err = features.Replay(engine, txns, func(t *data.Txn, row schema.Row) error {
		score, _, _ := scorer.Score(row)
		row.Num[risk] = float64(score)
		b.Append(row, backtest.Meta{ID: t.ID, DT: t.DT, Amount: t.Amount, Fraud: t.IsFraud})
		fraudByID[t.ID] = t.IsFraud
		if t.IsFraud == 1 {
			res.DatasetFraud++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	off := b.Table()
	f, err := os.Open(filepath.Join(e.work, "stream_decisions.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	top, err := stream.TableFromLog(f, cat, func(_ string, id int64) (int8, int64) { return fraudByID[id], backtest.NoLabelTime })
	if err != nil {
		return nil, err
	}
	res.Rows = top.N
	res.ColumnsIdentical = top.N == off.N
	if top.N == off.N {
		for c := range off.Num {
			for i := range off.Num[c] {
				if !same(off.Num[c][i], top.Num[c][i]) {
					res.CellsDiffer++
				}
			}
		}
		for c := range off.Str {
			for i := range off.Str[c] {
				if off.Dict[c][off.Str[c][i]] != top.Dict[c][top.Str[c][i]] {
					res.CellsDiffer++
				}
			}
		}
		for i := range off.ID {
			if off.ID[i] != top.ID[i] || off.DT[i] != top.DT[i] || !same(off.Amount[i], top.Amount[i]) {
				res.CellsDiffer++
			}
		}
		res.ColumnsIdentical = res.CellsDiffer == 0
	}

	listsJSON, err := os.ReadFile(e.lists)
	if err != nil {
		return nil, err
	}
	text, err := os.ReadFile(e.rules)
	if err != nil {
		return nil, err
	}
	current, err := service.CompileRules(cat, string(text), listsJSON, 1, "")
	if err != nil {
		return nil, err
	}
	bo, err := backtest.NewBacktester(off, current, 0)
	if err != nil {
		return nil, err
	}
	bt, err := backtest.NewBacktester(top, current, 0)
	if err != nil {
		return nil, err
	}
	var firstBlock *rules.CompiledRule
	for _, path := range ruleFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		set, err := service.CompileRules(cat, string(src), listsJSON, 1, "")
		if err != nil {
			return nil, err
		}
		for _, r := range set.Rules {
			ro, err := bo.Run(r, backtest.Options{})
			if err != nil {
				return nil, err
			}
			rt, err := bt.Run(r, backtest.Options{})
			if err != nil {
				return nil, err
			}
			res.RulesCompared++
			if stream.ReportJSON(ro) == stream.ReportJSON(rt) {
				res.ReportsIdentical++
			} else {
				res.ReportsDiffer = append(res.ReportsDiffer, r.Text)
			}
			if firstBlock == nil && r.Action == rules.Block && !r.Shadow {
				firstBlock = r
			}
		}
	}

	disputes, err := stream.ReadDisputes(ctx, []string{e.brokers}, e.prefix+".disputes")
	if err != nil {
		return nil, err
	}
	res.DisputesInTopic = len(disputes)
	if firstBlock != nil && len(disputes) > 0 {
		f2, err := os.Open(filepath.Join(e.work, "stream_decisions.jsonl"))
		if err != nil {
			return nil, err
		}
		defer f2.Close()
		lab, err := stream.TableFromLog(f2, cat, stream.DisputeLabels(disputes))
		if err != nil {
			return nil, err
		}
		bl, err := backtest.NewBacktester(lab, nil, 0)
		if err != nil {
			return nil, err
		}
		bd, err := backtest.NewBacktester(off, nil, 0)
		if err != nil {
			return nil, err
		}
		_, hi := lab.TimeSpan()
		opt := backtest.Options{AsOf: hi, UseLabelTime: true, Samples: -1}
		if rl, err := bl.Run(firstBlock, opt); err == nil {
			res.TopicLabelSummary = rl.SummaryText
		}
		if rd, err := bd.Run(firstBlock, backtest.Options{AsOf: hi, Samples: -1}); err == nil {
			res.DatasetLabelSummary = rd.SummaryText
		}
	}
	return res, nil
}
