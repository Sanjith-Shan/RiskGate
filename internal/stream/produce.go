package stream

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// The replay producer stands in for a payment service that publishes every
// payment to Kafka: it writes payments in event-time order, keyed and
// partitioned by card, with heartbeats, and the disputes a card network
// would send weeks later.

// ProduceConfig configures Produce.
type ProduceConfig struct {
	Topics Topics
	// Rate is payments per second, open loop: each payment has an intended
	// send time and is stamped with it, so a stalled producer shows up as
	// latency rather than hiding it. 0 sends as fast as Kafka accepts.
	Rate float64
	// HeartbeatEvery sends a heartbeat to every payments partition after this
	// many payments (and, at a fixed rate, at least every 250 ms).
	HeartbeatEvery int
	// Final sends the end-of-stream heartbeat (MaxPos) to every partition.
	Final bool
	// Disputes publishes a charge.dispute.created event for every fraudulent
	// payment, at its SIMULATED arrival time (backtest.DefaultDelay),
	// interleaved with the payments in event time.
	Disputes          bool
	DisputePartitions int32
	// Progress, if set, is incremented as payments are handed to the client.
	Progress *atomic.Int64
}

// ProduceStats reports a replay.
type ProduceStats struct {
	Payments, Heartbeats, Disputes int
	Took                           time.Duration
	// MaxBehind is how far the producer fell behind its schedule at worst.
	MaxBehind time.Duration
}

// PaymentValue is a payment's record value: the assess request body, the
// same JSON POST /v1/assess takes, without the label.
func PaymentValue(t *data.Txn) ([]byte, string, error) {
	ev := data.NewReplayEvent(t)
	b, err := json.Marshal(struct {
		PaymentID  string         `json:"payment_id"`
		Created    int64          `json:"created"`
		Amount     int64          `json:"amount"`
		Currency   string         `json:"currency"`
		RiskFields map[string]any `json:"risk_fields"`
	}{ev.PaymentID, ev.Created, ev.Amount, ev.Currency, ev.RiskFields})
	return b, ev.PaymentID, err
}

// DisputeEvent is the webhook event a fraudulent payment's dispute arrives
// as, in Clearinghouse's envelope.
func DisputeEvent(t *data.Txn, arrival int64) ([]byte, string, error) {
	pid := data.PaymentIDPrefix + strconv.FormatInt(t.ID, 10)
	obj, err := json.Marshal(webhook.Dispute{
		ID: "dp_" + strconv.FormatInt(t.ID, 10), Object: "dispute", PaymentIntent: pid,
		Amount: int64(t.Amount*100 + 0.5), Currency: "usd", Reason: "fraudulent", Status: "needs_response",
	})
	if err != nil {
		return nil, "", err
	}
	b, err := json.Marshal(webhook.Event{
		ID: "evt_dp_" + strconv.FormatInt(t.ID, 10), Object: "event", Type: webhook.TypeChargeDisputeCreated,
		Created: data.ReplayEpochUnix + arrival, APIVersion: "2026-09-01", Data: webhook.EventData{Object: obj},
	})
	return b, pid, err
}

// Produce writes txns, which must be in data.Less order, to the payments
// topic, and their disputes to the disputes topic.
func Produce(ctx context.Context, cl *kgo.Client, txns []data.Txn, nPayments int32, cfg ProduceConfig) (ProduceStats, error) {
	var st ProduceStats
	start := time.Now()
	var perr atomic.Pointer[error]
	done := func(_ *kgo.Record, err error) {
		if err != nil {
			perr.CompareAndSwap(nil, &err)
		}
	}
	heartbeat := func(p Pos, at time.Time) error {
		v, err := json.Marshal(Heartbeat(p))
		if err != nil {
			return err
		}
		for part := range nPayments {
			cl.Produce(ctx, &kgo.Record{
				Topic: cfg.Topics.Payments, Partition: part, Key: []byte("heartbeat"), Value: v, Timestamp: at,
				Headers: []kgo.RecordHeader{{Key: HeaderKind, Value: []byte(KindHeartbeat)}},
			}, done)
		}
		st.Heartbeats++
		return nil
	}

	// Disputes, in arrival order.
	type dispute struct {
		at int64
		i  int
	}
	var disputes []dispute
	if cfg.Disputes {
		for i := range txns {
			if txns[i].IsFraud != 1 {
				continue
			}
			if at, ok := backtest.DefaultDelay.LabelTime(txns[i].ID, txns[i].DT); ok {
				disputes = append(disputes, dispute{at, i})
			}
		}
		slices.SortFunc(disputes, func(a, b dispute) int {
			return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(txns[a.i].ID, txns[b.i].ID))
		})
	}
	nextDispute := 0
	sendDisputes := func(upTo int64, at time.Time) error {
		for ; nextDispute < len(disputes) && disputes[nextDispute].at <= upTo; nextDispute++ {
			d := disputes[nextDispute]
			v, pid, err := DisputeEvent(&txns[d.i], d.at)
			if err != nil {
				return err
			}
			cl.Produce(ctx, &kgo.Record{Topic: cfg.Topics.Disputes, Partition: PaymentPartition(pid, max(cfg.DisputePartitions, 1)), Key: []byte(pid), Value: v, Timestamp: at}, done)
			st.Disputes++
		}
		return nil
	}

	lastBeat := start
	for i := range txns {
		t := &txns[i]
		if i > 0 && !data.Less(&txns[i-1], t) {
			return st, fmt.Errorf("stream: payments not in (DT, ID) order at index %d", i)
		}
		at := time.Now()
		if cfg.Rate > 0 {
			at = start.Add(time.Duration(float64(i) / cfg.Rate * float64(time.Second)))
			if d := time.Until(at); d > 0 {
				select {
				case <-ctx.Done():
					return st, ctx.Err()
				case <-time.After(d):
				}
			} else if -d > st.MaxBehind {
				st.MaxBehind = -d
			}
		}
		if err := sendDisputes(t.DT, at); err != nil {
			return st, err
		}
		v, id, err := PaymentValue(t)
		if err != nil {
			return st, err
		}
		key := []byte(id)
		if card, ok := cardKey(t); ok {
			key = []byte(card)
		}
		cl.Produce(ctx, &kgo.Record{Topic: cfg.Topics.Payments, Partition: CardPartition(t, nPayments), Key: key, Value: v, Timestamp: at}, done)
		st.Payments++
		if cfg.Progress != nil {
			cfg.Progress.Add(1)
		}
		beat := cfg.HeartbeatEvery > 0 && st.Payments%cfg.HeartbeatEvery == 0
		if cfg.Rate > 0 && at.Sub(lastBeat) >= 250*time.Millisecond {
			beat = true
		}
		if beat {
			lastBeat = at
			if err := heartbeat(PosOf(t), at); err != nil {
				return st, err
			}
		}
		if p := perr.Load(); p != nil {
			return st, *p
		}
	}
	if cfg.Final {
		if err := sendDisputes(1<<62, time.Now()); err != nil {
			return st, err
		}
		if err := heartbeat(MaxPos, time.Now()); err != nil {
			return st, err
		}
	}
	if err := cl.Flush(ctx); err != nil {
		return st, err
	}
	if p := perr.Load(); p != nil {
		return st, *p
	}
	st.Took = time.Since(start)
	return st, nil
}

func cardKey(t *data.Txn) (string, bool) {
	if t.Card1 != t.Card1 { // NaN
		return "", false
	}
	return "card:" + strconv.FormatFloat(t.Card1, 'g', -1, 64), true
}
