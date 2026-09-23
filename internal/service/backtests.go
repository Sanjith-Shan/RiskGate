package service

import (
	"strconv"
	"strings"
	"sync"

	"github.com/Sanjith-Shan/RiskGate/internal/backtest"
	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

// backtests serves /v1/rules/test and /v1/rules/sweep from a feature table
// built offline (riskgate table) with the same feature engine and model the
// service runs.
//
// A backtest compares a proposed rule with the rules in force, so it needs
// the live rule set's decision on every row of the table. That costs one
// vectorized pass over the table, so the resulting Backtester is cached and
// rebuilt only when the live rule set or the labels change.
type backtests struct {
	base *backtest.Table // labels as in the dataset

	mu          sync.Mutex
	idIndex     map[int64]int32 // TransactionID -> row, built on first overlay
	cached      *backtest.Backtester
	cachedRules uint64
	cachedLbls  uint64
}

func newBacktests(t *backtest.Table) *backtests {
	if t == nil {
		return nil
	}
	return &backtests{base: t}
}

// get returns a Backtester for the given rule version, with online labels
// laid over the dataset's.
func (b *backtests) get(rv *ruleVersion, labels *LabelStore) (*backtest.Backtester, error) {
	lv := labels.Version()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cached != nil && b.cachedRules == rv.Version && b.cachedLbls == lv {
		return b.cached, nil
	}
	t := b.overlay(labels)
	bt, err := backtest.NewBacktester(t, rv.Set, 0)
	if err != nil {
		return nil, err
	}
	b.cached, b.cachedRules, b.cachedLbls = bt, rv.Version, lv
	return bt, nil
}

// overlay returns the base table, or a copy sharing every column except
// Fraud when online labels name payments in it. An online label is newer
// evidence than the dataset's: a dispute that arrived by webhook is the
// label, whatever the export said. Caller holds mu.
func (b *backtests) overlay(labels *LabelStore) *backtest.Table {
	t := b.base
	var fraud []int8
	labels.Known(func(paymentID string, label int8) {
		id, ok := transactionID(paymentID)
		if !ok {
			return
		}
		if b.idIndex == nil {
			b.idIndex = make(map[int64]int32, t.N)
			for i, x := range t.ID {
				b.idIndex[x] = int32(i)
			}
		}
		row, ok := b.idIndex[id]
		if !ok {
			return
		}
		if fraud == nil {
			fraud = append([]int8(nil), t.Fraud...)
		}
		fraud[row] = label
	})
	if fraud == nil {
		return t
	}
	return &backtest.Table{
		Catalog: t.Catalog, N: t.N,
		Num: t.Num, Str: t.Str, Dict: t.Dict,
		ID: t.ID, DT: t.DT, Amount: t.Amount, Fraud: fraud, LabelTime: t.LabelTime,
	}
}

// transactionID recovers the IEEE-CIS TransactionID from a replayed
// payment id ("txn_<id>"). Other ids are not in the table.
func transactionID(paymentID string) (int64, bool) {
	s, ok := strings.CutPrefix(paymentID, data.PaymentIDPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}
