package service

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/features"
)

func TestAssessContract(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()

	decisions := map[string]int{}
	matchedRule := false
	for _, p := range testStream(400, 1) {
		rec := do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body())
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		// Exactly the contract's fields, nothing more.
		var raw map[string]json.RawMessage
		decodeJSON(t, rec.Body.Bytes(), &raw)
		for _, k := range []string{"assessment_id", "decision", "risk_score", "matched_rule", "reasons", "ruleset_version"} {
			if _, ok := raw[k]; !ok {
				t.Fatalf("response lacks %q: %s", k, rec.Body)
			}
		}
		if len(raw) != 6 {
			t.Fatalf("response has extra fields: %s", rec.Body)
		}
		var r assessResponse
		decodeJSON(t, rec.Body.Bytes(), &r)
		decisions[r.Decision]++
		switch {
		case r.RiskScore < 0 || r.RiskScore > 99:
			t.Fatalf("risk_score %d out of range", r.RiskScore)
		case len(r.Reasons) > 3:
			t.Fatalf("%d reasons", len(r.Reasons))
		case r.RulesetVersion != 1:
			t.Fatalf("ruleset_version %d", r.RulesetVersion)
		case !strings.HasPrefix(r.AssessmentID, "asmt_") || len(r.AssessmentID) != assessmentIDLen:
			t.Fatalf("assessment_id %q", r.AssessmentID)
		}
		if r.MatchedRule != nil {
			matchedRule = true
			// A matched rule leads the reasons, by name.
			if len(r.Reasons) == 0 || !strings.Contains(r.Reasons[0], *r.MatchedRule) {
				t.Fatalf("matched %q but reasons are %q", *r.MatchedRule, r.Reasons)
			}
		} else if r.Decision != "allow" {
			t.Fatalf("decision %s without a rule", r.Decision)
		}
	}
	if decisions["allow"] == 0 || decisions["block"] == 0 || decisions["review"] == 0 || !matchedRule {
		t.Fatalf("stream should exercise every decision, got %v", decisions)
	}
}

func TestAssessRejectsBadRequests(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	h := s.Handler()
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"not json":           {`{`, 400},
		"no payment id":      {`{"created": 1525132800, "amount": 100}`, 400},
		"no created":         {`{"payment_id": "p", "amount": 100}`, 400},
		"negative amount":    {`{"payment_id": "p", "created": 1525132800, "amount": -1}`, 400},
		"unknown risk field": {`{"payment_id": "p", "created": 1525132800, "amount": 100, "risk_fields": {"card_1": 5}}`, 400},
		"wrong type":         {`{"payment_id": "p", "created": 1525132800, "amount": 100, "risk_fields": {"card1": "x"}}`, 400},
		"too large":          {`{"payment_id": "` + strings.Repeat("x", MaxAssessBody) + `"}`, 413},
		"null risk_fields":   {`{"payment_id": "p", "created": 1525132800, "amount": 100, "risk_fields": null}`, 200},
		"extra top-level":    {`{"payment_id": "p2", "created": 1525132800, "amount": 100, "is_fraud": 1}`, 200},
	} {
		if rec := do(t, h, http.MethodPost, "/v1/assess", []byte(tc.body)); rec.Code != tc.code {
			t.Errorf("%s: got %d, want %d: %s", name, rec.Code, tc.code, rec.Body)
		}
	}
	if rec := do(t, h, http.MethodGet, "/v1/assess", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/assess: %d", rec.Code)
	}
}

// A created far ahead of event time is refused before it reaches the
// velocity state, where it would become its keys' latest time and pin every
// later payment on them to it. Event time need not be wall time: the bound
// is the later of the latest accepted payment and the clock.
func TestAssessRejectsFutureCreated(t *testing.T) {
	env := newTestEnv(t)
	stream := testStream(3, 21)
	clock := &fixedClock{t: time.Unix(stream[0].Created, 0)}
	cfg := env.config(t)
	cfg.Now = clock.now
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check := func(p payment, want string) {
		t.Helper()
		rec := do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body())
		var e struct{ Error struct{ Code string } }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if want == "" && rec.Code != 200 || want != "" && (rec.Code != 400 || e.Error.Code != want) {
			t.Fatalf("created %d: %d %s, want %q", p.Created, rec.Code, rec.Body, want)
		}
	}
	shifted := func(p payment, by int64) payment { p.Created += by; return p }

	// Nothing accepted yet: the clock is the reference.
	check(shifted(stream[0], 2*86400), "created_in_future")
	check(stream[0], "")
	// Milliseconds are refused whatever the reference.
	check(shifted(stream[1], stream[1].Created*999), "created_out_of_range")
	// A simulated clock running ahead of the wall clock: the latest
	// accepted payment is the reference.
	clock.t = time.Unix(0, 0)
	bogus := shifted(stream[1], 25*3600)
	check(bogus, "created_in_future")
	if n := cardCount(s, bogus.Fields["card1"].(float64), bogus.Created+1); n != 0 {
		t.Fatalf("refused payment reached the velocity state (count %v)", n)
	}
	check(shifted(stream[1], 23*3600), "")
	check(stream[2], "") // late, but within the bound

	// A negative skew turns the bound off; the range check stays.
	cfg = newTestEnv(t).config(t)
	cfg.MaxFutureSkew = -1
	if s, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(shifted(stream[0], 10*365*86400), "")
	check(shifted(stream[0], stream[0].Created*999), "created_out_of_range")
}

// risk_fields.TransactionAmt is the exact dollar amount and wins over the
// rounded cents; without it the cents are used.
func TestAssessAmountPrecedence(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	req := &assessRequest{PaymentID: "txn_7", Created: data.ReplayEpochUnix + 86400, Amount: 1235,
		RiskFields: map[string]any{"TransactionAmt": 12.345}}
	tx, err := s.txnOf(req)
	if err != nil || tx.Amount != 12.345 || tx.ID != 7 || tx.DT != 86400 {
		t.Fatalf("with TransactionAmt: %+v, %v", tx, err)
	}
	req.RiskFields = map[string]any{}
	if tx, _ = s.txnOf(req); tx.Amount != 12.35 {
		t.Fatalf("from cents: %v", tx.Amount)
	}
}

// cardCount reads the live velocity state: payments seen on card1 = card
// in the hour before now.
func cardCount(s *Service, card float64, now int64) float64 {
	k, _ := features.CardKey(card)
	return s.engine.State().Read(k, data.DTFromUnix(now)).Count[0]
}

func TestIdempotentReplayDoesNotTouchVelocity(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	p := testStream(1, 3)[0]
	card := p.Fields["card1"].(float64)
	key := p.ID + ":1"

	first := do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body(), headerIdempotencyKey, key)
	second := do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body(), headerIdempotencyKey, key)
	if first.Code != 200 || second.Code != 200 || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("replay differs:\n%s\n%s", first.Body, second.Body)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("replay not marked")
	}
	if n := cardCount(s, card, p.Created+1); n != 1 {
		t.Fatalf("card counted %v times, want 1", n)
	}
	// A new attempt is a new key, and is assessed (and counted) again.
	do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body(), headerIdempotencyKey, p.ID+":2")
	if n := cardCount(s, card, p.Created+1); n != 2 {
		t.Fatalf("second attempt: card counted %v times, want 2", n)
	}
	// Same key, different body: rejected, not silently answered.
	other := p
	other.Cents++
	if rec := do(t, s.Handler(), http.MethodPost, "/v1/assess", other.body(), headerIdempotencyKey, key); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse: %d %s", rec.Code, rec.Body)
	}
	// A failed request is not stored: after fixing the body, the same key
	// is assessed.
	bad := do(t, s.Handler(), http.MethodPost, "/v1/assess", []byte(`{"payment_id":"x"}`), headerIdempotencyKey, "x:1")
	if bad.Code != 400 {
		t.Fatalf("bad body: %d", bad.Code)
	}
	good := payment{ID: "x", Created: p.Created, Cents: 100, Fields: map[string]any{}}
	if rec := do(t, s.Handler(), http.MethodPost, "/v1/assess", good.body(), headerIdempotencyKey, "x:1"); rec.Code != 200 {
		t.Fatalf("retry after a failure: %d %s", rec.Code, rec.Body)
	}
}

// Concurrent retries of one key: exactly one assessment, every caller gets
// its answer.
func TestIdempotencyConcurrent(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	p := testStream(1, 4)[0]
	var wg sync.WaitGroup
	bodies := make([][]byte, 16)
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body(), headerIdempotencyKey, "k:1")
			bodies[i] = rec.Body.Bytes()
		}()
	}
	wg.Wait()
	for _, b := range bodies[1:] {
		if !bytes.Equal(b, bodies[0]) {
			t.Fatalf("answers differ:\n%s\n%s", b, bodies[0])
		}
	}
	if n := cardCount(s, p.Fields["card1"].(float64), p.Created+1); n != 1 {
		t.Fatalf("card counted %v times", n)
	}
}

func TestIdempotencyTTLAndCapacity(t *testing.T) {
	clock := &fixedClock{}
	st := newIdemStore(10, 2, clock.now) // 10ns TTL, two entries
	claimDone := func(key string) {
		_, e, err := st.claim(t.Context(), key, 1)
		if err != nil || e == nil {
			t.Fatalf("claim %s: %v", key, err)
		}
		st.finish(e, []byte(key))
	}
	claimDone("a")
	claimDone("b")
	claimDone("c") // evicts a
	if st.len() != 2 {
		t.Fatalf("len %d", st.len())
	}
	if body, _, _ := st.claim(t.Context(), "b", 1); string(body) != "b" {
		t.Fatalf("b: %q", body)
	}
	clock.t = clock.t.Add(20)
	if body, e, _ := st.claim(t.Context(), "b", 1); body != nil || e == nil {
		t.Fatal("b should have expired")
	}
}

func TestAssessWithoutModel(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.config(t)
	cfg.Scorer = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := assess(t, s, testStream(1, 5)[0])
	if r.RiskScore != 0 {
		t.Fatalf("risk_score %d without a model", r.RiskScore)
	}
}

func TestAppendStringMatchesEncodingJSON(t *testing.T) {
	for _, s := range []string{"", "plain", `quote " and \ backslash`, "tab\tnl\ncr\r", "\x00\x1f\x7f", "<html>&", "héllo ✓", "bad \xff utf8", "  "} {
		var got string
		if err := json.Unmarshal(appendString(nil, s), &got); err != nil {
			t.Fatalf("%q: invalid JSON %s", s, appendString(nil, s))
		}
		want, _ := json.Marshal(s)
		var w string
		_ = json.Unmarshal(want, &w)
		if got != w {
			t.Errorf("%q: decoded %q, encoding/json gives %q", s, got, w)
		}
	}
	for _, v := range []float64{0, -0.5, 1e-300, math.MaxFloat64, 0.1 + 0.2} {
		var got float64
		if err := json.Unmarshal(appendFloat(nil, v), &got); err != nil || got != v {
			t.Errorf("%v round-trips as %v (%v)", v, got, err)
		}
	}
	if string(appendFloat(nil, math.NaN())) != "null" || string(appendFloat(nil, math.Inf(1))) != "null" {
		t.Error("NaN and Inf must be null")
	}
}

// A panic while assessing (net/http recovers it and carries on) must
// release the idempotency claim. Otherwise every retry of the key waits
// out its deadline and gets a 503, and the entry, never ready, is never
// expired and stops the expiry of everything claimed after it.
func TestIdempotencyClaimReleasedOnPanic(t *testing.T) {
	s := newTestEnv(t).start(t)
	defer s.Close()
	p := testStream(1, 4)[0]
	live := s.rules.current()
	s.rules.cur.Store(&ruleVersion{Version: live.Version}) // no rule set: evaluating panics
	func() {
		defer func() { _ = recover() }()
		do(t, s.Handler(), http.MethodPost, "/v1/assess", p.body(), headerIdempotencyKey, "k:1")
	}()
	s.rules.cur.Store(live)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/assess", bytes.NewReader(p.body()))
	req.Header.Set(headerIdempotencyKey, "k:1")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after a panic: %d %s", rec.Code, rec.Body)
	}
}
