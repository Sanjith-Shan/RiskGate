package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// Backtests replayed from the topics. Every decision line carries the full
// feature vector the decision was made on, risk_score included, so the
// decisions topic is a backtest table in another shape, and the disputes
// topic carries the labels with the time each arrived. A backtest built
// from them sees what production saw, including which fraud was known by
// when, instead of what a later offline export recomputes.

// Label is what the disputes topic says about one payment.
type Label struct {
	Fraud bool
	// ArrivedDT is when the first fraudulent dispute arrived, in
	// TransactionDT seconds.
	ArrivedDT int64
}

// ReadDisputes reads the disputes topic: every payment with a fraudulent
// dispute, and when the first one arrived.
func ReadDisputes(ctx context.Context, brokers []string, topic string) (map[string]Label, error) {
	out := map[string]Label{}
	err := ReadTopic(ctx, brokers, topic, func(r *kgo.Record) error {
		e, err := webhook.ParseEvent(r.Value)
		if err != nil {
			return nil // the labels stage dead-letters these; a backtest skips them
		}
		if e.Type != webhook.TypeChargeDisputeCreated {
			return nil
		}
		d, err := e.Dispute()
		if err != nil || d.Reason != "fraudulent" || d.PaymentIntent == "" {
			return nil
		}
		at := data.DTFromUnix(e.Created)
		if l, ok := out[d.PaymentIntent]; !ok || at < l.ArrivedDT {
			out[d.PaymentIntent] = Label{Fraud: true, ArrivedDT: at}
		}
		return nil
	})
	return out, err
}

// TableFromLog builds a backtest table from a decision log in event-time
// order (`riskgate stream decisions -out`). label gives each payment's
// label and label arrival time (backtest.NoLabelTime for none).
func TableFromLog(r io.Reader, cat *schema.Catalog, label func(paymentID string, id int64) (int8, int64)) (*backtest.Table, error) {
	amount := cat.MustLookup("amount").Slot
	b := backtest.NewBuilder(cat, 1<<16)
	var times []int64
	var prev data.Txn
	n := 0
	err := service.ReadLog(r, func(line int, e *service.LogEntry) error {
		row, err := e.Row(cat)
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(e.PaymentID, data.PaymentIDPrefix), 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: payment id %q is not %s<TransactionID>", line, e.PaymentID, data.PaymentIDPrefix)
		}
		cur := data.Txn{ID: id, DT: data.DTFromUnix(e.Created)}
		if n > 0 && !data.Less(&prev, &cur) {
			return fmt.Errorf("line %d: the log is not in event-time order", line)
		}
		prev = cur
		n++
		fraud, at := label(e.PaymentID, id)
		b.Append(row, backtest.Meta{ID: id, DT: cur.DT, Amount: row.Num[amount], Fraud: fraud})
		times = append(times, at)
		return nil
	})
	if err != nil {
		return nil, err
	}
	t := b.Table()
	t.LabelTime = times
	return t, t.Validate()
}

// DisputeLabels labels a payment fraud if the disputes topic has a
// fraudulent dispute for it and legitimate otherwise, with the dispute's
// arrival as its label time.
func DisputeLabels(m map[string]Label) func(string, int64) (int8, int64) {
	return func(pid string, _ int64) (int8, int64) {
		if l, ok := m[pid]; ok && l.Fraud {
			return backtest.Fraud, l.ArrivedDT
		}
		return backtest.Legit, backtest.NoLabelTime
	}
}

// ReportJSON renders a report for comparison: two backtests agree when
// their reports render the same.
func ReportJSON(r *backtest.Report) string {
	b, _ := json.Marshal(r)
	return string(b)
}
