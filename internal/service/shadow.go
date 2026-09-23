package service

import (
	"errors"
	"os"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

// Shadow rules.
//
// A shadow rule runs on every payment and is logged but never enforced. It
// is how a rule earns trust before it goes live: the report puts its online
// match rate since deployment next to what a backtest predicts for the
// same payments. The backtest runs the vectorized evaluator (the backtester's
// column engine, a separate implementation from the closures the service
// runs) over the feature vectors the decision log recorded. If the two ever
// disagree, the rule does something online that its backtest did not say,
// which is the one property RiskGate most needs to hold.

// ShadowRuleReport is one shadow rule's online and predicted match counts.
type ShadowRuleReport struct {
	shadowRecord
	OnlineRate *float64 `json:"online_rate"`
	// Predicted is the backtest over the logged decisions of every version
	// the rule has been live in.
	Predicted struct {
		Evaluated int      `json:"evaluated"`
		Matched   int      `json:"matched"`
		Rate      *float64 `json:"rate"`
	} `json:"predicted"`
	Agree bool `json:"agree"`
}

// ShadowReport is the body of GET /v1/rules/shadow.
type ShadowReport struct {
	RulesetVersion uint64             `json:"ruleset_version"`
	Rules          []ShadowRuleReport `json:"rules"`
	LogRecords     int                `json:"log_records_read"`
	LogDropped     uint64             `json:"log_dropped"`
	Note           string             `json:"note,omitempty"`
}

// ShadowReport compares every live shadow rule's online matches with a
// backtest of it over the decision log.
func (s *Service) ShadowReport() (*ShadowReport, error) {
	cur := s.rules.current()
	stats := s.rules.shadowRecords()
	rep := &ShadowReport{RulesetVersion: cur.Version, Rules: []ShadowRuleReport{}}
	_, rep.LogDropped, _, _ = s.dlog.stats()
	out := make([]ShadowRuleReport, len(stats))
	for i, st := range stats {
		out[i].shadowRecord = st
		out[i].OnlineRate = rate(int(st.Matched), int(st.Evaluated))
	}
	rep.Rules = out
	if len(stats) == 0 {
		return rep, nil
	}
	if s.dlog == nil {
		rep.Note = "the decision log is disabled, so there is nothing to backtest the shadow rules over"
		return rep, nil
	}

	// Read the log from the earliest version any shadow rule was live in.
	minVersion := stats[0].SinceVersion
	for _, st := range stats {
		minVersion = min(minVersion, st.SinceVersion)
	}
	s.dlog.Sync()
	f, err := os.Open(s.cfg.DecisionLog)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := backtest.NewBuilder(s.cat, 1024)
	var versions []uint64
	err = ReadLog(f, func(line int, e *LogEntry) error {
		if e.RulesetVersion < minVersion {
			return nil
		}
		row, err := e.Row(s.cat)
		if err != nil {
			return errLogLine(line, err)
		}
		amount := row.Num[s.cat.MustLookup("amount").Slot]
		b.Append(row, backtest.Meta{ID: int64(line), DT: data.DTFromUnix(e.Created), Amount: amount, Fraud: backtest.Unknown})
		versions = append(versions, e.RulesetVersion)
		return nil
	})
	if err != nil {
		return nil, err
	}
	t := b.Table()
	rep.LogRecords = t.N
	if rep.LogDropped > 0 {
		rep.Note = "the decision log dropped records, so the prediction covers fewer payments than the online count"
	}

	byID := map[string]*rules.CompiledRule{}
	for _, r := range cur.Set.Rules {
		byID[r.ID] = r
	}
	for i := range out {
		r := &out[i]
		cr := byID[r.ID]
		if cr == nil {
			return nil, errors.New("service: shadow rule missing from the live set")
		}
		prog, err := backtest.CompileVector(cr.Rule.Cond, t)
		if err != nil {
			return nil, err
		}
		match := prog.Eval()
		live := map[uint64]bool{}
		for _, v := range r.Versions {
			live[v] = true
		}
		for row, v := range versions {
			if !live[v] {
				continue
			}
			r.Predicted.Evaluated++
			if match.Get(row) {
				r.Predicted.Matched++
			}
		}
		r.Predicted.Rate = rate(r.Predicted.Matched, r.Predicted.Evaluated)
		r.Agree = uint64(r.Predicted.Matched) == r.Matched && uint64(r.Predicted.Evaluated) == r.Evaluated
	}
	return rep, nil
}

func rate(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	r := float64(num) / float64(den)
	return &r
}
