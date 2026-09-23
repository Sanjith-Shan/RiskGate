package service

import (
	"net/http"
	"strings"
	"testing"
)

func TestPageInfoHealthMetrics(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	h := s.Handler()

	rec := do(t, h, http.MethodGet, "/", nil)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "<title>RiskGate Rule Lab</title>") {
		t.Fatalf("page: %d %.200s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/nope", nil); rec.Code != 404 {
		t.Fatalf("unknown path: %d", rec.Code)
	}

	var info struct {
		Provenance string
		Model      bool
		Backtests  bool
		Attributes []map[string]string
	}
	decodeJSON(t, do(t, h, http.MethodGet, "/v1/info", nil).Body.Bytes(), &info)
	if info.Provenance != "SYNTHETIC DATA" || !info.Model || info.Backtests || len(info.Attributes) == 0 {
		t.Fatalf("info: %+v", info)
	}
	if rec := do(t, h, http.MethodPost, "/v1/rules/test", []byte(`{"rule": "block if :amount: > 1"}`)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("backtest without a table: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/healthz", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body)
	}

	for _, p := range testStream(300, 30) {
		assess(t, s, p, headerDeadline, "1000")
	}
	do(t, h, http.MethodPost, "/v1/assess", []byte(`{`))
	s.dlog.Sync()
	m := do(t, h, http.MethodGet, "/metrics", nil).Body.String()
	for _, want := range []string{
		`riskgate_http_requests_total{route="/v1/assess",code="200"} 300`,
		`riskgate_http_requests_total{route="/v1/assess",code="400"} 1`,
		`riskgate_assess_latency_seconds_count 300`,
		`riskgate_assess_latency_seconds_bucket{le="+Inf"} 300`,
		`riskgate_ruleset_version 1`,
		`riskgate_decision_log_written_total 300`,
		`riskgate_decision_log_dropped_total 0`,
		`riskgate_rule_decisions_total{rule_id="`,
		`riskgate_decisions_total{decision="block"}`,
		`riskgate_velocity_keys`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// Every sample line belongs to a family announced by # TYPE, and each
	// family's samples are contiguous, as the text format requires.
	seen, last := map[string]bool{}, ""
	for _, line := range strings.Split(strings.TrimSpace(m), "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			name := strings.Fields(line)[2]
			if seen[name] {
				t.Errorf("family %s announced twice", name)
			}
			seen[name], last = true, name
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.FieldsFunc(line, func(r rune) bool { return r == '{' || r == ' ' })[0]
		if name != last && !strings.HasPrefix(name, last+"_") {
			t.Errorf("sample %q outside its family (current %s)", line, last)
		}
	}
}
