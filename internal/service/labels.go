package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// Labels.
//
// Fraud labels reach RiskGate as Clearinghouse dispute webhooks, weeks after
// the payment. The label store records them so backtests count online
// outcomes (they overlay the dataset's labels) and so experiment 8 can
// compare decisions with what happened.
//
// Durability: the webhook handler acknowledges an event only after its
// label is appended and fsynced to the label log. Clearinghouse stops
// retrying at the first 2xx, so an acknowledged label that lived only in
// memory would be lost for good by a crash. The log is also what makes
// processing idempotent past the deduper's memory: every record upserts by
// dispute id, so a redelivered or replayed event changes nothing.
//
// The snapshot holds the labels as of its last sequence number, and startup
// replays only the log records after it (a checkpoint and a write-ahead
// log), so restart time does not grow with the log.

// LabelRecord is one line of the label log: one event, reduced to what a
// label needs.
type LabelRecord struct {
	Seq       uint64 `json:"seq"`
	EventID   string `json:"event_id"`
	Type      string `json:"type"`
	Created   int64  `json:"created"` // event time, Unix seconds
	PaymentID string `json:"payment_id"`
	DisputeID string `json:"dispute_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status,omitempty"`
	Amount    int64  `json:"amount,omitempty"`
}

// DisputeState is the latest known state of one dispute.
type DisputeState struct {
	Reason  string `json:"reason"`
	Status  string `json:"status"`
	Closed  bool   `json:"closed"`
	Amount  int64  `json:"amount"`
	Updated int64  `json:"updated"` // event time of the latest applied event
}

// PaymentLabel is everything known about one payment's outcome.
type PaymentLabel struct {
	Succeeded bool                     `json:"succeeded"`
	Disputes  map[string]*DisputeState `json:"disputes,omitempty"`
}

// Fraud reports the payment's label as the backtester counts it:
//
//   - fraud, if it has a dispute with reason "fraudulent" that the merchant
//     did not win; this is the IEEE-CIS notion of fraud, a reported
//     chargeback;
//   - legitimate, if every fraudulent dispute on it was closed as won,
//     meaning the merchant showed the payment was genuine;
//   - unknown otherwise. A succeeded payment with no dispute is not yet
//     evidence of anything, because disputes arrive weeks later (see
//     backtest.DefaultMaturity), so it does not override the dataset.
func (p *PaymentLabel) Fraud() int8 {
	label := backtest.Unknown
	for _, d := range p.Disputes {
		if d.Reason != "fraudulent" {
			continue
		}
		if d.Closed && d.Status == "won" {
			if label == backtest.Unknown {
				label = backtest.Legit
			}
			continue
		}
		return backtest.Fraud
	}
	return label
}

// LabelCounts summarises the store for /metrics.
type LabelCounts struct {
	Payments, Succeeded, Fraud, Legit, Disputes, ClosedDisputes int
}

// LabelStore is safe for concurrent use.
type LabelStore struct {
	mu       sync.Mutex
	log      *os.File // nil: in memory only
	seq      uint64
	payments map[string]*PaymentLabel
	version  uint64 // bumped on every change, to invalidate backtest overlays
}

// OpenLabelStore opens (creating if needed) the label log at path; "" keeps
// labels in memory only, for tests and demos.
func OpenLabelStore(path string) (*LabelStore, error) {
	s := &LabelStore{payments: map[string]*PaymentLabel{}}
	if path == "" {
		return s, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.log = f
	return s, nil
}

// replayLog applies every log record after the current sequence number.
// Called at startup, after any snapshot restore.
func (s *LabelStore) replayLog() error {
	if s.log == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.log.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReader(s.log)
	var offset int64
	for line := 1; ; line++ {
		b, err := br.ReadBytes('\n')
		if err == io.EOF {
			if len(b) > 0 {
				// A torn final line from a crash mid-append. That event
				// was never acknowledged, so Clearinghouse will redeliver
				// it; cut the fragment off so the next append starts on a
				// fresh line.
				return s.log.Truncate(offset)
			}
			return nil
		}
		if err != nil {
			return err
		}
		offset += int64(len(b))
		var r LabelRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("label log line %d: %w", line, err)
		}
		if r.Seq <= s.seq {
			continue // already in the snapshot
		}
		s.apply(&r)
		s.seq = r.Seq
	}
}

// Record turns a webhook event into a label and makes it durable. Events
// RiskGate does not use are ignored.
func (s *LabelStore) Record(e *webhook.Event) error {
	r := LabelRecord{EventID: e.ID, Type: e.Type, Created: e.Created}
	switch e.Type {
	case webhook.TypePaymentIntentSucceeded:
		pi, err := e.PaymentIntent()
		if err != nil {
			return err
		}
		r.PaymentID, r.Amount = pi.ID, pi.Amount
	case webhook.TypeChargeDisputeCreated, webhook.TypeChargeDisputeClosed:
		d, err := e.Dispute()
		if err != nil {
			return err
		}
		r.PaymentID, r.DisputeID, r.Reason, r.Status, r.Amount = d.PaymentIntent, d.ID, d.Reason, d.Status, d.Amount
		if r.DisputeID == "" {
			return errors.New("labels: dispute event without a dispute id")
		}
	default:
		return nil
	}
	if r.PaymentID == "" {
		return fmt.Errorf("labels: %s event %s names no payment", e.Type, e.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// The sequence number is spent even if the append fails: the line may
	// have reached the file anyway (a failed fsync), and replay skips any
	// record whose seq is not above the last one applied, so handing the
	// number out again would hide the next, acknowledged, label.
	s.seq++
	r.Seq = s.seq
	if s.log != nil {
		line, err := json.Marshal(&r)
		if err != nil {
			return err
		}
		if _, err := s.log.Write(append(line, '\n')); err != nil {
			return err
		}
		if err := s.log.Sync(); err != nil {
			return err
		}
	}
	s.apply(&r)
	return nil
}

// apply upserts r. Dispute events are keyed by dispute id and applied only
// if they are not older than what is stored, so redelivery and reordering
// (closed before created) converge on the same state.
func (s *LabelStore) apply(r *LabelRecord) {
	p := s.payments[r.PaymentID]
	if p == nil {
		p = &PaymentLabel{}
		s.payments[r.PaymentID] = p
	}
	s.version++
	if r.Type == webhook.TypePaymentIntentSucceeded {
		p.Succeeded = true
		return
	}
	if p.Disputes == nil {
		p.Disputes = map[string]*DisputeState{}
	}
	d := p.Disputes[r.DisputeID]
	if d == nil {
		d = &DisputeState{}
		p.Disputes[r.DisputeID] = d
	} else if r.Created < d.Updated {
		return // stale
	}
	closed := r.Type == webhook.TypeChargeDisputeClosed
	if d.Closed && !closed && r.Created == d.Updated {
		return // created and closed in the same second: closed wins
	}
	d.Reason, d.Status, d.Amount, d.Updated = r.Reason, r.Status, r.Amount, r.Created
	d.Closed = d.Closed || closed
}

// Label returns a copy of what is known about paymentID.
func (s *LabelStore) Label(paymentID string) (PaymentLabel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[paymentID]
	if !ok {
		return PaymentLabel{}, false
	}
	out := PaymentLabel{Succeeded: p.Succeeded}
	if p.Disputes != nil {
		out.Disputes = make(map[string]*DisputeState, len(p.Disputes))
		for id, d := range p.Disputes {
			c := *d
			out.Disputes[id] = &c
		}
	}
	return out, true
}

// Version changes whenever a label changes.
func (s *LabelStore) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Known calls fn for every payment with a fraud or legitimate label.
func (s *LabelStore) Known(fn func(paymentID string, label int8)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.payments {
		if l := p.Fraud(); l != backtest.Unknown {
			fn(id, l)
		}
	}
}

// Counts summarises the store.
func (s *LabelStore) Counts() LabelCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c LabelCounts
	c.Payments = len(s.payments)
	for _, p := range s.payments {
		if p.Succeeded {
			c.Succeeded++
		}
		switch p.Fraud() {
		case backtest.Fraud:
			c.Fraud++
		case backtest.Legit:
			c.Legit++
		}
		c.Disputes += len(p.Disputes)
		for _, d := range p.Disputes {
			if d.Closed {
				c.ClosedDisputes++
			}
		}
	}
	return c
}

type labelSnapshot struct {
	Seq      uint64                   `json:"seq"`
	Payments map[string]*PaymentLabel `json:"payments"`
}

func (s *LabelStore) snapshot() labelSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := labelSnapshot{Seq: s.seq, Payments: make(map[string]*PaymentLabel, len(s.payments))}
	for id, p := range s.payments {
		c := &PaymentLabel{Succeeded: p.Succeeded}
		if p.Disputes != nil {
			c.Disputes = make(map[string]*DisputeState, len(p.Disputes))
			for did, d := range p.Disputes {
				dc := *d
				c.Disputes[did] = &dc
			}
		}
		out.Payments[id] = c
	}
	return out
}

func (s *LabelStore) restore(snap labelSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq = snap.Seq
	s.payments = snap.Payments
	if s.payments == nil {
		s.payments = map[string]*PaymentLabel{}
	}
	s.version++
}

// Close closes the log.
func (s *LabelStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}
