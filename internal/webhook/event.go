package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Event types RiskGate consumes. Others are delivered to the callback too;
// it decides whether to ignore them.
const (
	TypePaymentIntentSucceeded = "payment_intent.succeeded"
	TypeChargeDisputeCreated   = "charge.dispute.created"
	TypeChargeDisputeClosed    = "charge.dispute.closed"
)

// Event is the envelope of every Clearinghouse webhook delivery.
type Event struct {
	ID         string    `json:"id"`
	Object     string    `json:"object"` // always "event"
	Type       string    `json:"type"`
	Created    int64     `json:"created"` // Unix seconds
	APIVersion string    `json:"api_version"`
	Data       EventData `json:"data"`
}

// EventData holds the resource the event is about. Object is left raw
// because its shape depends on Event.Type; decode it with PaymentIntent or
// Dispute.
type EventData struct {
	Object json.RawMessage `json:"object"`
}

// PaymentIntent is the data.object of payment_intent.* events. Amounts are
// in the currency's minor unit.
type PaymentIntent struct {
	ID             string `json:"id"`
	Amount         int64  `json:"amount"`
	AmountReceived int64  `json:"amount_received"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
}

// Dispute is the data.object of charge.dispute.* events.
type Dispute struct {
	ID            string `json:"id"`
	Object        string `json:"object"` // always "dispute"
	PaymentIntent string `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Reason        string `json:"reason"` // e.g. "fraudulent"
	Status        string `json:"status"`
}

// ParseEvent decodes and validates an event envelope. It does not verify
// the signature; call it only on a body that Verifier accepted.
func ParseEvent(body []byte) (*Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("webhook: decode event: %w", err)
	}
	switch {
	case e.Object != "event":
		return nil, fmt.Errorf("webhook: object is %q, want \"event\"", e.Object)
	case e.ID == "":
		return nil, errors.New("webhook: event has no id")
	case e.Type == "":
		return nil, errors.New("webhook: event has no type")
	case len(e.Data.Object) == 0 || string(e.Data.Object) == "null":
		return nil, errors.New("webhook: event has no data.object")
	}
	return &e, nil
}

// PaymentIntent decodes data.object for payment_intent.* events.
func (e *Event) PaymentIntent() (*PaymentIntent, error) {
	if !strings.HasPrefix(e.Type, "payment_intent.") {
		return nil, fmt.Errorf("webhook: event %s is %s, not a payment_intent event", e.ID, e.Type)
	}
	var pi PaymentIntent
	if err := json.Unmarshal(e.Data.Object, &pi); err != nil {
		return nil, fmt.Errorf("webhook: decode payment intent in %s: %w", e.ID, err)
	}
	return &pi, nil
}

// Dispute decodes data.object for charge.dispute.* events.
func (e *Event) Dispute() (*Dispute, error) {
	if !strings.HasPrefix(e.Type, "charge.dispute.") {
		return nil, fmt.Errorf("webhook: event %s is %s, not a dispute event", e.ID, e.Type)
	}
	var d Dispute
	if err := json.Unmarshal(e.Data.Object, &d); err != nil {
		return nil, fmt.Errorf("webhook: decode dispute in %s: %w", e.ID, err)
	}
	if d.Object != "dispute" {
		return nil, fmt.Errorf("webhook: event %s data.object is %q, want \"dispute\"", e.ID, d.Object)
	}
	return &d, nil
}
