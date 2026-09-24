package service

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func TestRulesPutAndGet(t *testing.T) {
	env := newTestEnv(t)
	s := env.start(t)
	defer s.Close()
	h := s.Handler()

	var got struct {
		Version  uint64
		Text     string
		Rules    []ruleJSON
		Warnings []diagJSON
	}
	decodeJSON(t, do(t, h, http.MethodGet, "/v1/rules", nil).Body.Bytes(), &got)
	if got.Version != 1 || got.Text != testRules || len(got.Rules) != 6 {
		t.Fatalf("GET: %+v", got)
	}

	// A bad rule set: 422, every error with carets, live rules unchanged.
	bad := "block if :card_txn_cnt_1h: >= 8\nreview if :amount: > \"300\"\n"
	rec := do(t, h, http.MethodPut, "/v1/rules", []byte(bad), "Content-Type", "text/plain")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad rules: %d %s", rec.Code, rec.Body)
	}
	var fail struct {
		Diagnostics []diagJSON
		Rendered    string
	}
	decodeJSON(t, rec.Body.Bytes(), &fail)
	if len(fail.Diagnostics) != 2 || !strings.Contains(fail.Rendered, "^^^") || !strings.Contains(fail.Rendered, "Did you mean :card_txn_count_1h:?") {
		t.Fatalf("diagnostics: %s", rec.Body)
	}
	if s.RulesetVersion() != 1 {
		t.Fatal("a rejected rule set changed the version")
	}

	// A good one, with new lists as JSON.
	body := `{"rules": "block if :purchaser_email_domain: in @risky\n", "lists": {"risky": ["anonymous.com"]}}`
	rec = do(t, h, http.MethodPut, "/v1/rules", []byte(body), "Content-Type", "application/json")
	if rec.Code != 200 || s.RulesetVersion() != 2 {
		t.Fatalf("good rules: %d %s", rec.Code, rec.Body)
	}
	r := assess(t, s, payment{ID: "p", Created: 1525132800, Cents: 100, Fields: map[string]any{"P_emaildomain": "anonymous.com"}})
	if r.Decision != "block" || r.RulesetVersion != 2 {
		t.Fatalf("after swap: %+v", r)
	}
	// Text only keeps the live lists.
	if rec = do(t, h, http.MethodPut, "/v1/rules", []byte("review if :purchaser_email_domain: in @risky"), "Content-Type", "text/plain"); rec.Code != 200 {
		t.Fatalf("text-only PUT: %d %s", rec.Code, rec.Body)
	}
	if rec = do(t, h, http.MethodPut, "/v1/rules", []byte(`{"rules": "", "lists": {"x": [1, "a"]}}`), "Content-Type", "application/json"); rec.Code != 400 {
		t.Fatalf("mixed list: %d %s", rec.Code, rec.Body)
	}
	// Every version is in the history directory, for audit.
	sets, err := LoadRuleHistory(env.config(t).RulesHistoryDir, schema.Default())
	if err != nil || len(sets) != 3 {
		t.Fatalf("history: %d versions, %v", len(sets), err)
	}
}

func TestRuleSwapIsAtomicUnderLoad(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	stream := testStream(2000, 9)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 20 {
			text := testRules
			if i%2 == 0 {
				text = "block if :amount: >= 0\n"
			}
			if _, err := s.rules.deploy(text, []byte(testLists)); err != nil {
				t.Error(err)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for _, p := range stream {
		r := assess(t, s, p)
		// Under the block-everything set every decision is a block, and it
		// must never be reported with another version's number.
		v, _ := s.rules.version(r.RulesetVersion)
		if v == nil {
			t.Fatalf("unknown version %d", r.RulesetVersion)
		}
		if strings.HasPrefix(v.Text, "block if :amount: >= 0") != (r.MatchedRule != nil && *r.MatchedRule == "block if :amount: >= 0") {
			t.Fatalf("version %d answered with rule %v", r.RulesetVersion, r.MatchedRule)
		}
	}
	<-done
}

func TestRulesTestEndpoint(t *testing.T) {
	env := newTestEnv(t)
	env.table = backtest.Synthetic(schema.Default(), backtest.SynthOptions{Rows: 20000, Seed: 1})
	s := env.start(t)
	defer s.Close()
	h := s.Handler()

	// Validate-only: errors as the author types, fast.
	rec := do(t, h, http.MethodPost, "/v1/rules/test", []byte(`{"rule": "block if :card_txn_cnt_1h: >= 8", "validate_only": true}`))
	var v struct {
		OK          bool
		Diagnostics []diagJSON
		Rendered    string
		ElapsedMs   float64 `json:"elapsed_ms"`
	}
	decodeJSON(t, rec.Body.Bytes(), &v)
	if rec.Code != 200 || v.OK || len(v.Diagnostics) != 1 || v.Diagnostics[0].Col != 10 || !strings.Contains(v.Rendered, "^") {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body)
	}
	if v.ElapsedMs > 5 {
		t.Logf("validate took %.2f ms (budget 5 ms; the machine may be loaded)", v.ElapsedMs)
	}

	// A backtest.
	rec = do(t, h, http.MethodPost, "/v1/rules/test", []byte(`{"rule": "block if :risk_score: >= 70", "from": "2017-12-15", "to": "2018-05-01"}`))
	var bt struct {
		OK      bool
		Summary string
		Report  backtest.Report
	}
	decodeJSON(t, rec.Body.Bytes(), &bt)
	if rec.Code != 200 || !bt.OK || bt.Summary == "" || bt.Report.Matched.All.Count == 0 || len(bt.Report.Samples) != 10 {
		t.Fatalf("backtest: %d %.500s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(bt.Report.From.Format(time.DateOnly), "2017-12-15") {
		t.Fatalf("from = %v", bt.Report.From)
	}
	// Two rules, or an invalid one, cannot be backtested.
	if rec = do(t, h, http.MethodPost, "/v1/rules/test", []byte(`{"rule": "block if :amount: > 1\nblock if :amount: > 2"}`)); rec.Code != 422 {
		t.Fatalf("two rules: %d", rec.Code)
	}
	if rec = do(t, h, http.MethodPost, "/v1/rules/test", []byte(`{"rule": "block if"}`)); rec.Code != 422 {
		t.Fatalf("invalid: %d", rec.Code)
	}

	// The sweep.
	rec = do(t, h, http.MethodGet, "/v1/rules/sweep", nil)
	var sw struct{ Sweep backtest.SweepResult }
	decodeJSON(t, rec.Body.Bytes(), &sw)
	if rec.Code != 200 || len(sw.Sweep.Points) != 100 {
		t.Fatalf("sweep: %d %.300s", rec.Code, rec.Body)
	}
}

// Online labels overlay the dataset's in backtests.
func TestBacktestLabelOverlay(t *testing.T) {
	tab := backtest.Synthetic(schema.Default(), backtest.SynthOptions{Rows: 100, Seed: 2})
	b := newBacktests(tab)
	ls, _ := OpenLabelStore("")
	if got := b.overlay(ls); got != tab {
		t.Fatal("no labels should mean no copy")
	}
	row := 7
	want := backtest.Fraud
	if tab.Fraud[row] == backtest.Fraud {
		want = backtest.Legit
	}
	ls.payments["txn_"+strconv.FormatInt(tab.ID[row], 10)] = labelFor(want)
	got := b.overlay(ls)
	if got.Fraud[row] != want || tab.Fraud[row] == want {
		t.Fatalf("overlay: row label %d, base %d", got.Fraud[row], tab.Fraud[row])
	}
}

func labelFor(l int8) *PaymentLabel {
	status := "needs_response"
	if l == backtest.Legit {
		status = "won"
	}
	return &PaymentLabel{Disputes: map[string]*DisputeState{"dp": {Reason: "fraudulent", Status: status, Closed: l == backtest.Legit}}}
}

// Every endpoint answers an oversized body with 413 body_too_large, as
// /v1/assess and PUT /v1/rules do, not with a JSON syntax error.
func TestRulesTestBodyTooLarge(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	body := []byte(`{"rule": "block if :amount: > 1", "validate_only": true, "pad": "` + strings.Repeat("x", maxTestBody) + `"}`)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/rules/test", body)
	var e struct{ Error struct{ Code string } }
	decodeJSON(t, rec.Body.Bytes(), &e)
	if rec.Code != http.StatusRequestEntityTooLarge || e.Error.Code != "body_too_large" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

// A JSON body without "rules" (for example {"rule": ...}, the spelling
// /v1/rules/test takes) is a client error. Deploying it as an empty rule
// set would switch every rule off.
func TestRulesPutJSONWithoutRules(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	before := s.RulesetVersion()
	rec := do(t, s.Handler(), http.MethodPut, "/v1/rules", []byte(`{"rule": "block if :amount: > 1"}`), "Content-Type", "application/json")
	if rec.Code != http.StatusBadRequest || s.RulesetVersion() != before || len(s.rules.current().Set.Rules) == 0 {
		t.Fatalf("got %d %s; live version %d, was %d", rec.Code, rec.Body, s.RulesetVersion(), before)
	}
}
