package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"

	"github.com/Sanjith-Shan/RiskGate/internal/model"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Audit replays a decision log: for every line it rebuilds the logged
// feature vector, re-scores it with the model, re-evaluates it with the rule
// set version that made the decision, and checks that the decision, the
// matched rule, the shadow matches, the risk score and the raw model score
// (to the bit) all come out the same. That is the claim "every decision can
// be replayed and explained", checked rather than asserted.

// AuditMismatch is one field of one decision that did not replay.
type AuditMismatch struct {
	Line         int    `json:"line"`
	AssessmentID string `json:"assessment_id"`
	Field        string `json:"field"`
	Logged       string `json:"logged"`
	Replayed     string `json:"replayed"`
}

// AuditReport summarises an audit.
type AuditReport struct {
	Records  int             `json:"records"`
	Verified int             `json:"verified"` // replayed identically
	Unscored int             `json:"unscored"` // logged without a model
	Versions map[uint64]int  `json:"versions"` // records per rule-set version
	Mismatch int             `json:"mismatched_records"`
	Examples []AuditMismatch `json:"examples"` // the first maxAuditExamples
}

const maxAuditExamples = 50

// OK reports whether every record replayed identically.
func (r *AuditReport) OK() bool { return r.Mismatch == 0 && r.Verified == r.Records }

func (r *AuditReport) String() string {
	return fmt.Sprintf("%d decisions, %d replayed identically, %d mismatched, %d rule-set versions", r.Records, r.Verified, r.Mismatch, len(r.Versions))
}

// Audit replays every decision in the log read from r. scorer may be nil
// only if no logged decision was scored. history maps a rule-set version to
// its rules (LoadRuleHistory).
func Audit(r io.Reader, cat *schema.Catalog, scorer *model.Scorer, history map[uint64]*HistoricalRules) (*AuditReport, error) {
	rep := &AuditReport{Versions: map[uint64]int{}, Examples: []AuditMismatch{}}
	risk := cat.MustLookup(schema.RiskScoreField.Name)
	stamp := catalogStamp(cat)
	var x, contrib []float64
	if scorer != nil {
		x, contrib = make([]float64, scorer.NumFeatures()), make([]float64, scorer.NumFeatures())
	}
	err := ReadLog(r, func(line int, e *LogEntry) error {
		rep.Records++
		rep.Versions[e.RulesetVersion]++
		bad := false
		mismatch := func(field, logged, replayed string) {
			bad = true
			if len(rep.Examples) < maxAuditExamples {
				rep.Examples = append(rep.Examples, AuditMismatch{line, e.AssessmentID, field, logged, replayed})
			}
		}
		defer func() {
			if bad {
				rep.Mismatch++
			} else {
				rep.Verified++
			}
		}()
		if e.Catalog != stamp {
			mismatch("catalog", e.Catalog, stamp)
			return nil
		}
		row, err := e.Row(cat)
		if err != nil {
			mismatch("features", err.Error(), "")
			return nil
		}

		// The model never reads risk_score (it is the model's output), but
		// the service scored the row before filling it, so do the same.
		loggedRisk := row.Num[risk.Slot]
		row.Num[risk.Slot] = math.NaN()
		switch {
		case !e.Scored:
			rep.Unscored++
			if !math.IsNaN(loggedRisk) {
				mismatch("risk_score feature", fmtFloat(loggedRisk), "missing (unscored)")
			}
		case scorer == nil:
			mismatch("model", "scored", "no model given to the audit")
			return nil
		default:
			score, _, raw := scorer.Score(row)
			if score != e.RiskScore {
				mismatch("risk_score", strconv.Itoa(e.RiskScore), strconv.Itoa(score))
			}
			if e.RawScore == nil || math.Float64bits(*e.RawScore) != math.Float64bits(raw) {
				mismatch("raw_score", fmtPtr(e.RawScore), fmtFloat(raw))
			}
			if loggedRisk != float64(e.RiskScore) {
				mismatch("risk_score feature", fmtFloat(loggedRisk), strconv.Itoa(e.RiskScore))
			}
			// Contributions are logged for explanation; check that the
			// logged ones are what the model gives for this row.
			scorer.Contributions(row, x, contrib)
			for i, name := range scorer.FeatureNames() {
				if v, ok := e.Contributions[name]; ok && math.Float64bits(v) != math.Float64bits(contrib[i]) {
					mismatch("contribution "+name, fmtFloat(v), fmtFloat(contrib[i]))
				}
			}
			row.Num[risk.Slot] = float64(score)
		}

		h, ok := history[e.RulesetVersion]
		if !ok {
			mismatch("ruleset_version", strconv.FormatUint(e.RulesetVersion, 10), "not in the rule history")
			return nil
		}
		d := h.Set.Evaluate(row)
		if d.Action.String() != e.Decision {
			mismatch("decision", e.Decision, d.Action.String())
		}
		replayedRule := ""
		if d.Rule != nil {
			replayedRule = d.Rule.ID
		}
		if loggedRule := deref(e.RuleID); loggedRule != replayedRule {
			mismatch("rule_id", loggedRule, replayedRule)
		}
		if got := shadowIDs(d); !slices.Equal(got, e.ShadowMatches) && !(len(got) == 0 && len(e.ShadowMatches) == 0) {
			mismatch("shadow_matches", fmt.Sprint(e.ShadowMatches), fmt.Sprint(got))
		}
		return nil
	})
	return rep, err
}

func shadowIDs(d rules.Decision) []string {
	out := make([]string, len(d.Shadow))
	for i, r := range d.Shadow {
		out[i] = r.ID
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func fmtPtr(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmtFloat(*v)
}

// readJSONL calls fn for each line. A final line without a newline that
// does not parse is a torn write from a crash and is ignored; fn sees every
// complete line.
func readJSONL(r io.Reader, fn func(line int, b []byte) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for line := 1; ; line++ {
		b, err := br.ReadBytes('\n')
		if err == io.EOF {
			if len(bytes.TrimSpace(b)) > 0 && json.Valid(b) {
				return fn(line, b)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		if err := fn(line, b); err != nil {
			return err
		}
	}
}

func unmarshalLine(b []byte, v any) error { return json.Unmarshal(b, v) }

func errLogLine(line int, err error) error { return fmt.Errorf("decision log line %d: %w", line, err) }

func errFeatureCount(got, want int) error {
	return fmt.Errorf("logged %d features, catalog has %d", got, want)
}

func errFeatureType(name string, v any) error {
	return fmt.Errorf("feature %s: unexpected %T", name, v)
}
