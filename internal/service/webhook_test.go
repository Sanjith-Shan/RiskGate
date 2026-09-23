package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

func eventBody(id, typ string, created int64, object any) []byte {
	obj, _ := json.Marshal(object)
	b, _ := json.Marshal(map[string]any{
		"id": id, "object": "event", "type": typ, "created": created, "api_version": "2026-09-01",
		"data": map[string]json.RawMessage{"object": obj},
	})
	return b
}

func dispute(id, pi, reason, status string) map[string]any {
	return map[string]any{"id": id, "object": "dispute", "payment_intent": pi, "amount": 1999, "currency": "usd", "reason": reason, "status": status}
}

// post sends a signed (or deliberately mis-signed) webhook over a real
// HTTP connection.
func post(t *testing.T, url string, body []byte, secret string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/webhooks/clearinghouse", bytes.NewReader(body))
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(body, time.Now(), []byte(secret)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Status string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Status
}

func TestWebhookEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	s := env.start(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	created := time.Now().Unix()
	fraud := eventBody("evt_1", webhook.TypeChargeDisputeCreated, created, dispute("dp_1", "txn_3000001", "fraudulent", "needs_response"))
	if code, st := post(t, srv.URL, fraud, testSecret); code != 200 || st != "processed" {
		t.Fatalf("signed event: %d %s", code, st)
	}
	// At-least-once delivery: the same event again is acknowledged and not
	// applied twice.
	if code, st := post(t, srv.URL, fraud, testSecret); code != 200 || st != "duplicate" {
		t.Fatalf("duplicate: %d %s", code, st)
	}
	if code, _ := post(t, srv.URL, eventBody("evt_x", webhook.TypeChargeDisputeCreated, created, dispute("dp_9", "txn_9", "fraudulent", "")), "wrong_secret"); code != 400 {
		t.Fatalf("bad signature: %d", code)
	}
	succeeded := eventBody("evt_2", webhook.TypePaymentIntentSucceeded, created, map[string]any{"id": "txn_3000002", "amount": 500, "currency": "usd", "status": "succeeded"})
	if code, _ := post(t, srv.URL, succeeded, testSecret); code != 200 {
		t.Fatal(code)
	}
	// A fraudulent dispute the merchant won makes the payment legitimate.
	post(t, srv.URL, eventBody("evt_3", webhook.TypeChargeDisputeCreated, created, dispute("dp_2", "txn_3000003", "fraudulent", "needs_response")), testSecret)
	post(t, srv.URL, eventBody("evt_4", webhook.TypeChargeDisputeClosed, created+10, dispute("dp_2", "txn_3000003", "fraudulent", "won")), testSecret)

	c := s.labels.Counts()
	if c.Fraud != 1 || c.Legit != 1 || c.Succeeded != 1 || c.Disputes != 2 || c.ClosedDisputes != 1 {
		t.Fatalf("label counts %+v", c)
	}
	if s.dedupe.Duplicates() != 1 {
		t.Fatalf("dedupe hits %d", s.dedupe.Duplicates())
	}
	resp, _ := http.Get(srv.URL + "/metrics")
	var m bytes.Buffer
	_, _ = m.ReadFrom(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"riskgate_webhook_duplicates_total 1", `riskgate_labels{kind="fraud"} 1`, `riskgate_http_requests_total{route="/v1/webhooks/clearinghouse",code="400"} 1`} {
		if !strings.Contains(m.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// The label log is durable before the 200: a new store replaying it
	// sees the same labels.
	srv.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ls, err := OpenLabelStore(env.config(t).LabelLog)
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	if err := ls.replayLog(); err != nil {
		t.Fatal(err)
	}
	if got := ls.Counts(); got != c {
		t.Fatalf("replayed %+v, want %+v", got, c)
	}
}

func TestRefusesToBootWithoutSecrets(t *testing.T) {
	env := newTestEnv(t)
	for _, secrets := range [][]string{nil, {""}, {"ok", ""}} {
		cfg := env.config(t)
		cfg.WebhookSecrets = secrets
		if _, err := New(cfg); err == nil {
			t.Errorf("booted with secrets %q", secrets)
		}
	}
}

// Dispute events converge whatever order they arrive in.
func TestLabelUpsertOrder(t *testing.T) {
	created := func(ts int64, status string) *webhook.Event {
		e, _ := webhook.ParseEvent(eventBody(fmt.Sprint("evt_c", ts), webhook.TypeChargeDisputeCreated, ts, dispute("dp", "txn_1", "fraudulent", status)))
		return e
	}
	closed, _ := webhook.ParseEvent(eventBody("evt_closed", webhook.TypeChargeDisputeClosed, 20, dispute("dp", "txn_1", "fraudulent", "won")))
	for _, order := range [][]*webhook.Event{{created(10, "needs_response"), closed}, {closed, created(10, "needs_response")}} {
		ls, _ := OpenLabelStore("")
		for _, e := range order {
			if err := ls.Record(e); err != nil {
				t.Fatal(err)
			}
		}
		p, _ := ls.Label("txn_1")
		if p.Fraud() != backtest.Legit || !p.Disputes["dp"].Closed {
			t.Fatalf("got %+v", p.Disputes["dp"])
		}
	}
}

func TestLabelLogTornLine(t *testing.T) {
	path := t.TempDir() + "/labels.jsonl"
	ls, _ := OpenLabelStore(path)
	e, _ := webhook.ParseEvent(eventBody("evt_1", webhook.TypeChargeDisputeCreated, 1, dispute("dp", "txn_1", "fraudulent", "")))
	if err := ls.Record(e); err != nil {
		t.Fatal(err)
	}
	ls.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":2,"event_id":"evt_2","ty`) // crash mid-append
	f.Close()

	ls, _ = OpenLabelStore(path)
	if err := ls.replayLog(); err != nil {
		t.Fatal(err)
	}
	e2, _ := webhook.ParseEvent(eventBody("evt_2", webhook.TypeChargeDisputeCreated, 2, dispute("dp2", "txn_2", "fraudulent", "")))
	if err := ls.Record(e2); err != nil {
		t.Fatal(err)
	}
	ls.Close()
	ls, _ = OpenLabelStore(path)
	defer ls.Close()
	if err := ls.replayLog(); err != nil {
		t.Fatalf("log unreadable after a torn write: %v", err)
	}
	if c := ls.Counts(); c.Fraud != 2 {
		t.Fatalf("counts %+v", c)
	}
}
