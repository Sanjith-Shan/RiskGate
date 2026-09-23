package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// The shadow report's online counts equal a backtest of the same rules,
// by the vectorized evaluator, over the logged feature vectors, exactly.
func TestShadowOnlineEqualsBacktest(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	stream := testStream(1500, 11)
	for _, p := range stream[:700] {
		assess(t, s, p)
	}
	// Redeploy with one shadow rule kept and one new: the kept one keeps
	// counting across versions, the new one starts at this version.
	next := strings.Replace(testRules, "shadow review if :amount: > 150", "shadow review if :card_amount_sum_24h: > 5000", 1)
	if _, err := s.rules.deploy(next, []byte(testLists)); err != nil {
		t.Fatal(err)
	}
	for _, p := range stream[700:] {
		assess(t, s, p)
	}

	rep, err := s.ShadowReport()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 2 {
		t.Fatalf("%d shadow rules", len(rep.Rules))
	}
	for _, r := range rep.Rules {
		t.Logf("%s: online %d/%d, backtest %d/%d", r.Text, r.Matched, r.Evaluated, r.Predicted.Matched, r.Predicted.Evaluated)
		if !r.Agree || r.Matched == 0 || r.Matched == r.Evaluated {
			t.Errorf("%s: online %d/%d, backtest %d/%d", r.Text, r.Matched, r.Evaluated, r.Predicted.Matched, r.Predicted.Evaluated)
		}
		wantEvaluated := uint64(1500)
		if r.SinceVersion == 2 {
			wantEvaluated = 800
		}
		if r.Evaluated != wantEvaluated {
			t.Errorf("%s evaluated %d, want %d", r.Text, r.Evaluated, wantEvaluated)
		}
	}
	rec := do(t, s.Handler(), http.MethodGet, "/v1/rules/shadow", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"agree":true`) {
		t.Fatalf("GET /v1/rules/shadow: %d %s", rec.Code, rec.Body)
	}
}

// Every logged decision replays identically against the logged rule-set
// version and the model, across a rule change; a tampered line is caught.
func TestAuditReplaysEveryDecision(t *testing.T) {
	env := newTestEnv(t)
	s := env.start(t)
	stream := testStream(1200, 12)
	for i, p := range stream {
		if i == 600 {
			if rec := do(t, s.Handler(), http.MethodPut, "/v1/rules", []byte("block if :risk_score: >= 40\nshadow review if :amount: > 100\n"), "Content-Type", "text/plain"); rec.Code != 200 {
				t.Fatal(rec.Body)
			}
		}
		assess(t, s, p)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := env.config(t)
	history, err := LoadRuleHistory(cfg.RulesHistoryDir, schema.Default())
	if err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(cfg.DecisionLog)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Audit(bytes.NewReader(logBytes), schema.Default(), env.scorer, history)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Records != len(stream) || len(rep.Versions) != 2 {
		t.Fatalf("%v: %+v", rep, rep.Examples)
	}

	// Change one logged feature so that the decision no longer follows.
	lines := bytes.Split(bytes.TrimSpace(logBytes), []byte("\n"))
	var e map[string]any
	for i, line := range lines {
		_ = json.Unmarshal(line, &e)
		if e["decision"] == "block" && e["ruleset_version"].(float64) == 2 {
			e["decision"] = "allow"
			lines[i], _ = json.Marshal(e)
			break
		}
	}
	rep, err = Audit(bytes.NewReader(bytes.Join(lines, []byte("\n"))), schema.Default(), env.scorer, history)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Mismatch != 1 || rep.Examples[0].Field != "decision" {
		t.Fatalf("tampered log passed: %v %+v", rep, rep.Examples)
	}
}

// Restored state equals the state at shutdown, part by part.
func TestSnapshotRestoresState(t *testing.T) {
	env := newTestEnv(t)
	s := env.start(t)
	for i, p := range testStream(800, 13) {
		assess(t, s, p, headerIdempotencyKey, fmt.Sprintf("%s:%d", p.ID, i%2))
	}
	if rec := do(t, s.Handler(), http.MethodPut, "/v1/rules", []byte(testRules+"review if :amount: > 250\n"), "Content-Type", "text/plain"); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	e, _ := webhook.ParseEvent(eventBody("evt_1", webhook.TypeChargeDisputeCreated, 1, dispute("dp", "txn_3000001", "fraudulent", "")))
	if err := s.labels.Record(e); err != nil {
		t.Fatal(err)
	}
	s.dedupe.Begin("evt_1")
	s.dedupe.Commit("evt_1")

	var before bytes.Buffer
	if err := s.engine.State().Snapshot(&before); err != nil {
		t.Fatal(err)
	}
	wantIdem, wantDedupe, wantLabels, wantShadow := s.idem.snapshot(), s.dedupe.Snapshot(), s.labels.snapshot(), s.rules.shadowRecords()
	wantVersion, wantText := s.RulesetVersion(), s.rules.current().Text
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r := env.start(t)
	defer r.Close()
	var after bytes.Buffer
	if err := r.engine.State().Snapshot(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("velocity state differs after restore")
	}
	same := func(name string, a, b any) {
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) {
			t.Errorf("%s differs after restore:\n%s\n%s", name, ja, jb)
		}
	}
	same("idempotency", wantIdem, r.idem.snapshot())
	same("dedupe", wantDedupe, r.dedupe.Snapshot())
	same("labels", wantLabels, r.labels.snapshot())
	same("shadow stats", wantShadow, r.rules.shadowRecords())
	if r.RulesetVersion() != wantVersion || r.rules.current().Text != wantText {
		t.Errorf("rules: version %d, want %d", r.RulesetVersion(), wantVersion)
	}
	// Older versions come back from the history, for audit and replay.
	if _, ok := r.rules.version(1); !ok {
		t.Error("version 1 missing after restart")
	}
}

// Decisions after a restart equal those of a run that never stopped, over
// the same event stream, through the HTTP handler; retries of keys
// answered before the restart get the stored answer.
func TestRestartMatchesUninterruptedRun(t *testing.T) {
	stream := testStream(1600, 14)
	half := 900
	key := func(p payment) []string { return []string{headerIdempotencyKey, p.ID + ":1"} }

	uninterrupted := newTestEnv(t).start(t)
	defer uninterrupted.Close()
	var want []assessResponse
	for _, p := range stream {
		want = append(want, assess(t, uninterrupted, p, key(p)...).decisionOnly())
	}

	env := newTestEnv(t)
	s := env.start(t)
	var firstHalf []assessResponse
	for _, p := range stream[:half] {
		firstHalf = append(firstHalf, assess(t, s, p, key(p)...))
	}
	if err := s.Close(); err != nil { // graceful shutdown: final snapshot
		t.Fatal(err)
	}
	restarted := env.start(t)
	defer restarted.Close()
	for i, p := range stream[half:] {
		got := assess(t, restarted, p, key(p)...).decisionOnly()
		if !reflect.DeepEqual(got, want[half+i]) {
			t.Fatalf("payment %d after restart:\n got %+v\nwant %+v", half+i, got, want[half+i])
		}
	}
	// A Clearinghouse retry of a pre-restart payment: same answer, same
	// assessment id, no second count.
	p := stream[half-1]
	if got := assess(t, restarted, p, key(p)...); got.AssessmentID != firstHalf[half-1].AssessmentID {
		t.Fatalf("retry after restart re-assessed: %+v vs %+v", got, firstHalf[half-1])
	}
}

// A snapshot of another state configuration is refused rather than
// silently dropped.
func TestSnapshotMismatchRefused(t *testing.T) {
	env := newTestEnv(t)
	s := env.start(t)
	assess(t, s, testStream(1, 15)[0])
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := env.config(t)
	cfg.State, cfg.StateDesc, _ = NewState("bucketed", "sharded", 8)
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "velocity state") {
		t.Fatalf("mismatched state accepted: %v", err)
	}
	// Corruption is caught by the checksum.
	b, _ := os.ReadFile(cfg.SnapshotPath)
	b[len(b)/2] ^= 0xff
	os.WriteFile(cfg.SnapshotPath, b, 0o644)
	if _, err := New(env.config(t)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupt snapshot accepted: %v", err)
	}
}

// The periodic snapshot loop writes and the metric reports its age.
func TestPeriodicSnapshots(t *testing.T) {
	env := newTestEnv(t)
	s := env.start(t)
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.RunSnapshots(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.lastSnapshot.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-loopDone
	if _, err := os.Stat(filepath.Join(env.dir, "riskgate.snap")); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s.Handler(), http.MethodGet, "/metrics", nil); !strings.Contains(rec.Body.String(), "riskgate_snapshot_age_seconds") {
		t.Fatal("no snapshot age metric")
	}
}

// When the writer stalls, the decision log drops and counts records
// instead of blocking the payment path.
func TestDecisionLogDropsInsteadOfBlocking(t *testing.T) {
	r, w, err := os.Pipe() // nobody reads r until the end: writes stall
	if err != nil {
		t.Fatal(err)
	}
	cat := schema.Default()
	l := newDecisionLog(w, cat, nil, 0, 4, time.Hour)
	const n = 5000
	start := time.Now()
	for range n {
		rec := l.get()
		rec.paymentID = strings.Repeat("p", 200)
		l.put(rec)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("put blocked for %v", el)
	}
	written, dropped, _, queued := l.stats()
	if dropped == 0 {
		t.Fatalf("nothing dropped (written %d, queued %d)", written, queued)
	}
	go func() { _, _ = bytes.NewBuffer(nil).ReadFrom(r) }() // unstall
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	written, dropped, _, _ = l.stats()
	if written+dropped != n {
		t.Fatalf("written %d + dropped %d != %d", written, dropped, n)
	}
}
