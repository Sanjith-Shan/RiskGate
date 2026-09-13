package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

const (
	maxRulesBody = 1 << 20
	maxTestBody  = 256 << 10
)

// diagJSON is a rules.Diagnostic for the API. Rendered is the caret form
// the page shows in a <pre>.
type diagJSON struct {
	Severity string `json:"severity"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"end_line"`
	EndCol   int    `json:"end_col"`
	Message  string `json:"message"`
	Rendered string `json:"rendered"`
}

func diagsJSON(ds rules.Diagnostics) []diagJSON {
	out := make([]diagJSON, len(ds))
	for i, d := range ds {
		out[i] = diagJSON{
			Severity: d.Severity.String(),
			Line:     d.Span.Start.Line, Col: d.Span.Start.Col,
			EndLine: d.Span.End.Line, EndCol: d.Span.End.Col,
			Message: d.Message(), Rendered: d.Render(),
		}
	}
	return out
}

// ruleJSON describes one compiled rule.
type ruleJSON struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	Line   int    `json:"line"`
	Action string `json:"action"`
	Shadow bool   `json:"shadow"`
	Text   string `json:"text"`
}

func rulesJSON(rs *rules.RuleSet) []ruleJSON {
	out := make([]ruleJSON, len(rs.Rules))
	for i, r := range rs.Rules {
		out[i] = ruleJSON{ID: r.ID, Index: r.Index, Line: r.Line, Action: r.Action.String(), Shadow: r.Shadow, Text: r.Text}
	}
	return out
}

func (s *Service) rulesetJSON(rv *ruleVersion) map[string]any {
	return map[string]any{
		"version":           rv.Version,
		"deployed_at":       rv.DeployedAt,
		"text":              rv.Text,
		"lists":             rv.ListsJSON,
		"rules":             rulesJSON(rv.Set),
		"warnings":          diagsJSON(rv.Set.Warnings),
		"rendered_warnings": rv.Set.Warnings.Render(),
	}
}

// GET /v1/rules: the live rule set.
func (s *Service) handleRulesGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.rulesetJSON(s.rules.current()))
}

// PUT /v1/rules: validate, compile and atomically swap in a rule set.
//
// The body is the rule text (text/plain), or {"rules": "...", "lists":
// {...}} (application/json) to replace the named lists too. Without lists
// the live lists are kept. Errors come back as 422 with every diagnostic,
// rendered with carets; the live rule set is untouched.
func (s *Service) handleRulesPut(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRulesBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	text, lists := string(body), []byte(nil)
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		var req struct {
			Rules *string         `json:"rules"`
			Lists json.RawMessage `json:"lists"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		// Without "rules" (a typo such as "rule"), deploying would switch
		// every rule off. An empty string still clears the rules on purpose.
		if req.Rules == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", `the JSON body needs "rules", the rule text`)
			return
		}
		text, lists = *req.Rules, req.Lists
	}
	if lists == nil {
		lists = s.rules.current().ListsJSON
	}
	rv, err := s.rules.deploy(text, lists)
	var ds rules.Diagnostics
	var le *listsError
	switch {
	case errors.As(err, &ds):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       map[string]string{"code": "invalid_rules", "message": "the rule set has errors; the live rules are unchanged"},
			"diagnostics": diagsJSON(ds),
			"rendered":    ds.Render(),
		})
		return
	case errors.As(err, &le):
		writeError(w, http.StatusBadRequest, "invalid_lists", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "deploy_failed", err.Error())
		return
	}
	s.log.Info("rules deployed", "version", rv.Version, "rules", len(rv.Set.Rules), "warnings", len(rv.Set.Warnings))
	writeJSON(w, http.StatusOK, s.rulesetJSON(rv))
}

// testRequest is the body of POST /v1/rules/test.
type testRequest struct {
	Rule string `json:"rule"`
	// ValidateOnly checks the rule and returns diagnostics without a
	// backtest: the page calls it as the author types.
	ValidateOnly bool `json:"validate_only"`
	// From and To bound the backtest in event time: Unix seconds, a
	// YYYY-MM-DD date, or RFC 3339. Both optional.
	From json.RawMessage `json:"from"`
	To   json.RawMessage `json:"to"`
	// MaturityDays overrides backtest.DefaultMaturity; negative disables it.
	MaturityDays *float64 `json:"maturity_days"`
}

// POST /v1/rules/test: validate a rule, and unless validate_only, backtest
// it against the live rule set.
func (s *Service) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTestBody)).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	started := time.Now()
	cur := s.rules.current()
	rs, _, err := s.rules.compile(req.Rule, cur.ListsJSON, 0)
	var ds rules.Diagnostics
	if err != nil && !errors.As(err, &ds) {
		writeError(w, http.StatusInternalServerError, "compile_failed", err.Error())
		return
	}
	if err == nil {
		ds = rs.Warnings
	}
	resp := map[string]any{
		"ok":          err == nil,
		"diagnostics": diagsJSON(ds),
		"rendered":    ds.Render(),
	}
	if err == nil && len(rs.Rules) != 1 {
		resp["ok"] = false
		resp["error"] = fmt.Sprintf("write exactly one rule to test; this has %d", len(rs.Rules))
		err = errors.New("not one rule")
	}
	if req.ValidateOnly {
		resp["elapsed_ms"] = msSince(started)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}
	if s.bt == nil {
		writeError(w, http.StatusServiceUnavailable, "no_table", "backtests need a feature table; start the service with -table (build one with `riskgate table`)")
		return
	}
	opt, err := backtestOptions(req.From, req.To, req.MaturityDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	bt, err := s.bt.get(cur, s.labels)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backtest_failed", err.Error())
		return
	}
	report, err := bt.Run(rs.Rules[0], opt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "backtest_failed", err.Error())
		return
	}
	resp["report"] = report
	resp["summary"] = report.SummaryText
	resp["ruleset_version"] = cur.Version
	resp["provenance"] = s.cfg.Provenance
	resp["elapsed_ms"] = msSince(started)
	writeJSON(w, http.StatusOK, resp)
}

// GET /v1/rules/sweep?from=&to=&given_current=true: precision and recall of
// `block if :risk_score: >= k` for every k.
func (s *Service) handleSweep(w http.ResponseWriter, r *http.Request) {
	if s.bt == nil {
		writeError(w, http.StatusServiceUnavailable, "no_table", "the sweep needs a feature table; start the service with -table")
		return
	}
	q := r.URL.Query()
	quote := func(v string) json.RawMessage {
		if v == "" {
			return nil
		}
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			return json.RawMessage(v)
		}
		b, _ := json.Marshal(v)
		return b
	}
	opt, err := backtestOptions(quote(q.Get("from")), quote(q.Get("to")), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	cur := s.rules.current()
	bt, err := s.bt.get(cur, s.labels)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backtest_failed", err.Error())
		return
	}
	res, err := bt.Sweep(opt, q.Get("given_current") != "false")
	if err != nil {
		writeError(w, http.StatusBadRequest, "sweep_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sweep": res, "ruleset_version": cur.Version, "provenance": s.cfg.Provenance})
}

// backtestOptions converts API times (event time) to TransactionDT.
func backtestOptions(from, to json.RawMessage, maturityDays *float64) (backtest.Options, error) {
	var opt backtest.Options
	var err error
	if opt.From, err = parseEventTime(from); err != nil {
		return opt, fmt.Errorf("from: %w", err)
	}
	if opt.To, err = parseEventTime(to); err != nil {
		return opt, fmt.Errorf("to: %w", err)
	}
	if maturityDays != nil {
		opt.Maturity = time.Duration(*maturityDays * float64(24*time.Hour))
		if *maturityDays == 0 {
			opt.Maturity = -1 // 0 means "no maturity window" to a caller
		}
	}
	return opt, nil
}

// parseEventTime reads Unix seconds, YYYY-MM-DD or RFC 3339 and returns
// TransactionDT, or 0 (meaning "the table's own bound") when absent.
func parseEventTime(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return data.DTFromUnix(n), nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, errors.New("want Unix seconds or a date")
	}
	if str == "" {
		return 0, nil
	}
	for _, layout := range []string{time.DateOnly, time.RFC3339} {
		if t, err := time.Parse(layout, str); err == nil {
			return data.DTFromUnix(t.Unix()), nil
		}
	}
	return 0, fmt.Errorf("%q is not YYYY-MM-DD, RFC 3339 or Unix seconds", str)
}

// GET /v1/rules/shadow: every live shadow rule's online match rate next to
// what the backtest over the same decisions predicts. See shadowReport.
func (s *Service) handleShadow(w http.ResponseWriter, r *http.Request) {
	rep, err := s.ShadowReport()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "shadow_report_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// GET /v1/info: what the page needs to label itself.
func (s *Service) handleInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"provenance":      s.cfg.Provenance,
		"ruleset_version": s.rules.current().Version,
		"model":           s.scorer != nil,
		"backtests":       s.bt != nil,
		"attributes":      s.attributes(),
	}
	if s.bt != nil {
		lo, hi := s.bt.base.TimeSpan()
		info["table_rows"] = s.bt.base.N
		info["table_from"] = backtest.DefaultEpoch.Add(time.Duration(lo) * time.Second)
		info["table_to"] = backtest.DefaultEpoch.Add(time.Duration(hi) * time.Second)
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Service) attributes() []map[string]string {
	out := make([]map[string]string, 0, len(s.cat.Fields()))
	for _, f := range s.cat.Fields() {
		out = append(out, map[string]string{"name": f.Name, "kind": f.Kind.String(), "doc": f.Doc})
	}
	return out
}

// GET /healthz.
func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"ruleset_version": s.rules.current().Version,
		"model":           s.scorer != nil,
	})
}

// GET /metrics.
func (s *Service) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = s.metrics.write(w, s.rules.ruleCounts(), s.gauges())
}

// gauges gathers the metrics other components own.
func (s *Service) gauges() []gauge {
	cur := s.rules.current()
	g := []gauge{
		{name: "riskgate_ruleset_version", help: "Version of the live rule set.", typ: "gauge", value: float64(cur.Version)},
		{name: "riskgate_webhook_duplicates_total", help: "Webhook deliveries absorbed by event-id dedupe.", typ: "counter", value: float64(s.dedupe.Duplicates())},
		{name: "riskgate_webhook_dedupe_entries", help: "Webhook event ids remembered.", typ: "gauge", value: float64(s.dedupe.Len())},
		{name: "riskgate_webhook_dedupe_evictions_total", help: "Event ids forgotten early because the deduper was full.", typ: "counter", value: float64(s.dedupe.Evictions())},
		{name: "riskgate_idempotency_entries", help: "Assess responses held for idempotent replay.", typ: "gauge", value: float64(s.idem.len())},
	}
	if at := s.lastSnapshot.Load(); at > 0 {
		age := s.now().Sub(time.Unix(0, at)).Seconds()
		g = append(g, gauge{name: "riskgate_snapshot_age_seconds", help: "Seconds since the last successful snapshot.", typ: "gauge", value: age})
	}
	lc := s.labels.Counts()
	for _, kv := range []struct {
		kind string
		n    int
	}{{"payments", lc.Payments}, {"succeeded", lc.Succeeded}, {"fraud", lc.Fraud}, {"legit", lc.Legit}, {"disputes", lc.Disputes}, {"disputes_closed", lc.ClosedDisputes}} {
		g = append(g, gauge{name: "riskgate_labels", help: "Online labels recorded from webhooks, by kind.", typ: "gauge", labels: `{kind="` + kv.kind + `"}`, value: float64(kv.n)})
	}
	written, dropped, errs, queued := s.dlog.stats()
	g = append(g,
		gauge{name: "riskgate_decision_log_written_total", help: "Decision log records written.", typ: "counter", value: float64(written)},
		gauge{name: "riskgate_decision_log_dropped_total", help: "Decision log records dropped because the writer could not keep up (the payment was not delayed).", typ: "counter", value: float64(dropped)},
		gauge{name: "riskgate_decision_log_write_errors_total", help: "Decision log write errors.", typ: "counter", value: float64(errs)},
		gauge{name: "riskgate_decision_log_queued", help: "Decision log records waiting to be written.", typ: "gauge", value: float64(queued)},
	)
	st := s.engine.State().Stats()
	g = append(g,
		gauge{name: "riskgate_velocity_keys", help: "Live velocity keys (-1 when the state cannot count them).", typ: "gauge", value: float64(st.Keys)},
		gauge{name: "riskgate_velocity_bytes", help: "Estimated velocity state heap bytes.", typ: "gauge", value: float64(st.Bytes)},
	)
	return g
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
