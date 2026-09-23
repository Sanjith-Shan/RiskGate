package backtest

import (
	"fmt"
	"math"
	"sync"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Label values in Table.Fraud.
const (
	Legit   int8 = 0
	Fraud   int8 = 1
	Unknown int8 = -1
)

// NoLabelTime is the LabelTime of a payment whose fraud label never arrives:
// every legitimate payment, and a fraudulent one the simulation decided is
// never disputed.
const NoLabelTime = math.MaxInt64

// Table is a column store of feature rows, the backtester's view of history.
//
// Columns follow the catalog's slots: Num[f.Slot] holds number field f with
// NaN for missing, and Str[f.Slot] holds string field f as dictionary codes
// into Dict[f.Slot], where code 0 is always "" (missing). Dictionary coding
// is what makes string predicates cheap: a test such as
// lower(:purchaser_email_domain:) = "gmail.com" is decided once per distinct
// value (a few hundred) instead of once per row (590K), and the per-row work
// is a table lookup on a uint32.
//
// The metadata columns are not rule attributes. They are what a backtest
// needs to turn matches into money and outcomes: the payment's ID, its event
// time (TransactionDT, seconds), its amount in dollars, its label (Fraud,
// Legit or Unknown), and optionally LabelTime, the time its fraud label
// arrived, which only the label-delay simulation fills in (nil otherwise).
//
// A Table is immutable once built, and safe for concurrent readers.
type Table struct {
	Catalog *schema.Catalog
	N       int

	Num  [][]float64
	Str  [][]uint32
	Dict [][]string

	ID        []int64
	DT        []int64
	Amount    []float64
	Fraud     []int8
	LabelTime []int64

	lowerMu    sync.Mutex
	lowerCache map[int][]string // lowered dictionaries, built on first use
}

// Meta is the per-row metadata a Builder appends alongside the features.
type Meta struct {
	ID     int64
	DT     int64
	Amount float64
	Fraud  int8
}

// Builder appends rows to a Table in order.
type Builder struct {
	t     *Table
	codes []map[string]uint32
}

// NewBuilder returns a builder for rows of cat. capacity is a hint.
func NewBuilder(cat *schema.Catalog, capacity int) *Builder {
	t := &Table{
		Catalog: cat,
		Num:     make([][]float64, cat.NumCount()),
		Str:     make([][]uint32, cat.StrCount()),
		Dict:    make([][]string, cat.StrCount()),
		ID:      make([]int64, 0, capacity),
		DT:      make([]int64, 0, capacity),
		Amount:  make([]float64, 0, capacity),
		Fraud:   make([]int8, 0, capacity),
	}
	b := &Builder{t: t, codes: make([]map[string]uint32, cat.StrCount())}
	for i := range t.Num {
		t.Num[i] = make([]float64, 0, capacity)
	}
	for i := range t.Str {
		t.Str[i] = make([]uint32, 0, capacity)
		t.Dict[i] = []string{""}
		b.codes[i] = map[string]uint32{"": 0}
	}
	return b
}

// Append adds one row. The row must have been built for the builder's
// catalog (schema.Catalog.NewRow).
func (b *Builder) Append(row schema.Row, m Meta) {
	t := b.t
	if len(row.Num) != len(t.Num) || len(row.Str) != len(t.Str) {
		panic(fmt.Sprintf("backtest: row has %d numbers and %d strings, catalog has %d and %d",
			len(row.Num), len(row.Str), len(t.Num), len(t.Str)))
	}
	for i, v := range row.Num {
		t.Num[i] = append(t.Num[i], v)
	}
	for i, s := range row.Str {
		code, ok := b.codes[i][s]
		if !ok {
			code = uint32(len(t.Dict[i]))
			b.codes[i][s] = code
			t.Dict[i] = append(t.Dict[i], s)
		}
		t.Str[i] = append(t.Str[i], code)
	}
	t.ID = append(t.ID, m.ID)
	t.DT = append(t.DT, m.DT)
	t.Amount = append(t.Amount, m.Amount)
	t.Fraud = append(t.Fraud, m.Fraud)
	t.N++
}

// Len returns the number of rows appended so far.
func (b *Builder) Len() int { return b.t.N }

// Table returns the table built so far. The builder must not be used after.
func (b *Builder) Table() *Table {
	t := b.t
	b.t = nil
	return t
}

// Row materializes row i into dst, which must come from Catalog.NewRow. It
// allocates nothing: strings are shared with the dictionary.
func (t *Table) Row(i int, dst *schema.Row) {
	for c, col := range t.Num {
		dst.Num[c] = col[i]
	}
	for c, col := range t.Str {
		dst.Str[c] = t.Dict[c][col[i]]
	}
}

// Rows materializes every row, for the row-at-a-time evaluator. A row store
// of the full dataset is a few hundred megabytes, which is the point of
// comparison: the columns are the compact form.
func (t *Table) Rows() []schema.Row {
	out := make([]schema.Row, t.N)
	nums := make([]float64, t.N*len(t.Num))
	strs := make([]string, t.N*len(t.Str))
	for i := range out {
		out[i] = schema.Row{
			Num: nums[i*len(t.Num) : (i+1)*len(t.Num) : (i+1)*len(t.Num)],
			Str: strs[i*len(t.Str) : (i+1)*len(t.Str) : (i+1)*len(t.Str)],
		}
		t.Row(i, &out[i])
	}
	return out
}

// Validate checks the table's internal consistency: column lengths, codes
// inside their dictionaries, code 0 meaning missing and no other code
// meaning it, and labels in range. Load calls it, so a corrupt or hand-built
// table fails loudly instead of producing a wrong backtest.
func (t *Table) Validate() error {
	if t.Catalog == nil {
		return fmt.Errorf("backtest: table has no catalog")
	}
	if len(t.Num) != t.Catalog.NumCount() || len(t.Str) != t.Catalog.StrCount() || len(t.Dict) != len(t.Str) {
		return fmt.Errorf("backtest: table has %d number and %d string columns, catalog has %d and %d",
			len(t.Num), len(t.Str), t.Catalog.NumCount(), t.Catalog.StrCount())
	}
	for name, n := range map[string]int{"ID": len(t.ID), "DT": len(t.DT), "Amount": len(t.Amount), "Fraud": len(t.Fraud)} {
		if n != t.N {
			return fmt.Errorf("backtest: column %s has %d rows, table has %d", name, n, t.N)
		}
	}
	if t.LabelTime != nil && len(t.LabelTime) != t.N {
		return fmt.Errorf("backtest: LabelTime has %d rows, table has %d", len(t.LabelTime), t.N)
	}
	for c, col := range t.Num {
		if len(col) != t.N {
			return fmt.Errorf("backtest: number column %d has %d rows, table has %d", c, len(col), t.N)
		}
	}
	for c, col := range t.Str {
		if len(col) != t.N {
			return fmt.Errorf("backtest: string column %d has %d rows, table has %d", c, len(col), t.N)
		}
		d := t.Dict[c]
		if len(d) == 0 || d[0] != "" {
			return fmt.Errorf("backtest: dictionary %d does not start with the missing value", c)
		}
		for code, s := range d[1:] {
			if s == "" {
				return fmt.Errorf("backtest: dictionary %d has a second empty entry at code %d", c, code+1)
			}
		}
		for i, code := range col {
			if int(code) >= len(d) {
				return fmt.Errorf("backtest: row %d of string column %d has code %d, dictionary has %d entries", i, c, code, len(d))
			}
		}
	}
	for i, f := range t.Fraud {
		if f != Fraud && f != Legit && f != Unknown {
			return fmt.Errorf("backtest: row %d has label %d", i, f)
		}
	}
	return nil
}

// RowsInTime returns the rows whose DT is in [from, to).
func (t *Table) RowsInTime(from, to int64) *Bitmap {
	b := NewBitmap(t.N)
	for i, dt := range t.DT {
		if dt >= from && dt < to {
			b.Set(i)
		}
	}
	return b
}

// TimeSpan returns the smallest and largest DT, or 0, 0 for an empty table.
func (t *Table) TimeSpan() (lo, hi int64) {
	if t.N == 0 {
		return 0, 0
	}
	lo, hi = t.DT[0], t.DT[0]
	for _, dt := range t.DT {
		lo, hi = min(lo, dt), max(hi, dt)
	}
	return lo, hi
}
