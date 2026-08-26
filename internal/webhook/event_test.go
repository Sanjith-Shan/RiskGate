package webhook

import "testing"

func TestParsePaymentIntentEvent(t *testing.T) {
	body := `{"id":"evt_pi","object":"event","type":"payment_intent.succeeded","created":1767225600,"api_version":"2026-01-01","data":{"object":{"id":"pi_1","amount":1250,"amount_received":1250,"currency":"usd","status":"succeeded"}}}`
	e, err := ParseEvent([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "evt_pi" || e.Type != TypePaymentIntentSucceeded || e.Created != 1767225600 || e.APIVersion != "2026-01-01" {
		t.Fatalf("envelope %+v", e)
	}
	pi, err := e.PaymentIntent()
	if err != nil {
		t.Fatal(err)
	}
	want := PaymentIntent{ID: "pi_1", Amount: 1250, AmountReceived: 1250, Currency: "usd", Status: "succeeded"}
	if *pi != want {
		t.Fatalf("got %+v, want %+v", *pi, want)
	}
	if _, err := e.Dispute(); err == nil {
		t.Fatal("Dispute() on a payment_intent event should fail")
	}
}

func TestParseDisputeEvents(t *testing.T) {
	for _, typ := range []string{TypeChargeDisputeCreated, TypeChargeDisputeClosed} {
		body := `{"id":"evt_d","object":"event","type":"` + typ + `","created":1,"api_version":"2026-01-01","data":{"object":{"id":"dp_1","object":"dispute","payment_intent":"pi_1","amount":4999,"currency":"usd","reason":"fraudulent","status":"lost"}}}`
		e, err := ParseEvent([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		d, err := e.Dispute()
		if err != nil {
			t.Fatal(err)
		}
		want := Dispute{ID: "dp_1", Object: "dispute", PaymentIntent: "pi_1", Amount: 4999, Currency: "usd", Reason: "fraudulent", Status: "lost"}
		if *d != want {
			t.Fatalf("%s: got %+v", typ, *d)
		}
		if _, err := e.PaymentIntent(); err == nil {
			t.Fatal("PaymentIntent() on a dispute event should fail")
		}
	}
}

func TestEventObjectDecodeErrors(t *testing.T) {
	cases := []Event{
		{ID: "e", Type: TypeChargeDisputeCreated, Data: EventData{Object: []byte(`{"object":"charge"}`)}},
		{ID: "e", Type: TypeChargeDisputeCreated, Data: EventData{Object: []byte(`{"amount":"lots"}`)}},
	}
	for _, e := range cases {
		if _, err := e.Dispute(); err == nil {
			t.Errorf("Dispute() accepted %s", e.Data.Object)
		}
	}
	bad := Event{ID: "e", Type: TypePaymentIntentSucceeded, Data: EventData{Object: []byte(`[]`)}}
	if _, err := bad.PaymentIntent(); err == nil {
		t.Error("PaymentIntent() accepted an array")
	}
}
