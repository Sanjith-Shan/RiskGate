package features

import (
	"fmt"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Replay drives history through the engine: for each transaction in order,
// ScoreAndUpdate (score first, then add, so a payment never sees itself or
// anything after it), then fn. The row passed to fn is reused for the next
// transaction; clone it to keep it. fn may be nil, which just builds state.
//
// txns must be strictly increasing under data.Less. Replay checks this as it
// goes, because an out-of-order input would silently leak the future into
// the features.
func Replay(e *Engine, txns []data.Txn, fn func(t *data.Txn, row schema.Row) error) error {
	row := e.NewRow()
	for i := range txns {
		t := &txns[i]
		if i > 0 && !data.Less(&txns[i-1], t) {
			return fmt.Errorf("features: replay input not in (DT, TransactionID) order at index %d (ID %d after ID %d)",
				i, t.ID, txns[i-1].ID)
		}
		e.ScoreAndUpdate(t, row)
		if fn != nil {
			if err := fn(t, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// Columns is a replay's output stored column-wise: Num[slot][i] and
// Str[slot][i] are row i's values, in the input's order. This is the layout
// a vectorized backtest scans.
type Columns struct {
	Num [][]float64
	Str [][]string
}

// ReplayColumns replays txns and keeps every row, column-wise.
func ReplayColumns(e *Engine, txns []data.Txn) (*Columns, error) {
	cat := e.Catalog()
	c := &Columns{Num: make([][]float64, cat.NumCount()), Str: make([][]string, cat.StrCount())}
	for i := range c.Num {
		c.Num[i] = make([]float64, 0, len(txns))
	}
	for i := range c.Str {
		c.Str[i] = make([]string, 0, len(txns))
	}
	err := Replay(e, txns, func(_ *data.Txn, row schema.Row) error {
		for i, v := range row.Num {
			c.Num[i] = append(c.Num[i], v)
		}
		for i, v := range row.Str {
			c.Str[i] = append(c.Str[i], v)
		}
		return nil
	})
	return c, err
}
