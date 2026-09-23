package backtest

import (
	"bytes"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

func TestBitmap(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	for _, n := range []int{0, 1, 63, 64, 65, 130, 1000} {
		a, b := NewBitmap(n), NewBitmap(n)
		ref := make([][2]bool, n)
		for i := range n {
			if rng.IntN(2) == 0 {
				a.Set(i)
				ref[i][0] = true
			}
			if rng.IntN(3) == 0 {
				b.Set(i)
				ref[i][1] = true
			}
		}
		check := func(name string, got *Bitmap, f func(x, y bool) bool) {
			t.Helper()
			want := 0
			for i := range n {
				w := f(ref[i][0], ref[i][1])
				if got.Get(i) != w {
					t.Fatalf("n=%d %s: row %d = %v", n, name, i, got.Get(i))
				}
				if w {
					want++
				}
			}
			if got.Count() != want || len(got.Rows()) != want {
				t.Fatalf("n=%d %s: count %d, want %d", n, name, got.Count(), want)
			}
			// Nothing past N, ever.
			if len(got.W) > 0 && got.W[len(got.W)-1]&^tailMask(n) != 0 {
				t.Fatalf("n=%d %s: bits past N", n, name)
			}
		}
		check("and", a.Clone().And(b), func(x, y bool) bool { return x && y })
		check("or", a.Clone().Or(b), func(x, y bool) bool { return x || y })
		check("andnot", a.Clone().AndNot(b), func(x, y bool) bool { return x && !y })
		check("not", a.Clone().Not(), func(x, _ bool) bool { return !x })
		check("fill", a.Clone().Fill(), func(bool, bool) bool { return true })
		check("reset", a.Clone().Reset(), func(bool, bool) bool { return false })
		if a.AndCount(b) != a.Clone().And(b).Count() {
			t.Fatal("AndCount")
		}
		if !a.Equal(a.Clone()) || (n > 0 && a.Equal(a.Clone().Not())) || a.Equal(NewBitmap(n+1)) {
			t.Fatal("Equal")
		}
		// Next walks the same rows as ForEach.
		var viaNext []int
		for i := a.Next(-5); i >= 0; i = a.Next(i + 1) {
			viaNext = append(viaNext, i)
		}
		if got := a.Rows(); len(got) != len(viaNext) {
			t.Fatalf("Next found %d rows, ForEach %d", len(viaNext), len(got))
		}
		if a.Any() != (a.Count() > 0) {
			t.Fatal("Any")
		}
		if n > 0 {
			a.Set(n - 1)
			a.Clear(n - 1)
			if a.Get(n - 1) {
				t.Fatal("Clear")
			}
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("mismatched sizes did not panic")
		}
	}()
	NewBitmap(3).And(NewBitmap(4))
}

func TestSaveLoadRoundTrip(t *testing.T) {
	cat := schema.Default()
	tbl := Synthetic(cat, SynthOptions{Rows: 777, Seed: 3, Edge: 0.2, Missing: 0.2})
	tbl.LabelTime = SimulateLabelTimes(tbl, DefaultDelay)
	path := filepath.Join(t.TempDir(), "features.rgt")
	if err := tbl.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, cat)
	if err != nil {
		t.Fatal(err)
	}
	if got.N != tbl.N {
		t.Fatalf("N %d, want %d", got.N, tbl.N)
	}
	for c := range tbl.Num {
		for i, v := range tbl.Num[c] {
			if math.Float64bits(v) != math.Float64bits(got.Num[c][i]) {
				t.Fatalf("num col %d row %d: %v != %v (bit patterns must survive: -0, NaN)", c, i, got.Num[c][i], v)
			}
		}
	}
	rowA, rowB := cat.NewRow(), cat.NewRow()
	for i := range tbl.N {
		tbl.Row(i, &rowA)
		got.Row(i, &rowB)
		for c := range rowA.Str {
			if rowA.Str[c] != rowB.Str[c] {
				t.Fatalf("row %d str %d: %q != %q", i, c, rowB.Str[c], rowA.Str[c])
			}
		}
		if tbl.ID[i] != got.ID[i] || tbl.DT[i] != got.DT[i] || tbl.Amount[i] != got.Amount[i] ||
			tbl.Fraud[i] != got.Fraud[i] || tbl.LabelTime[i] != got.LabelTime[i] {
			t.Fatalf("row %d metadata differs", i)
		}
	}

	// A table without label times round-trips without them.
	tbl.LabelTime = nil
	var buf bytes.Buffer
	if err := tbl.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if got, err := Decode(buf.Bytes(), cat); err != nil || got.LabelTime != nil {
		t.Fatalf("decode without label times: %v", err)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	cat := schema.Default()
	tbl := Synthetic(cat, SynthOptions{Rows: 100, Seed: 3})
	var buf bytes.Buffer
	if err := tbl.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()

	flipped := bytes.Clone(good)
	flipped[len(flipped)/2] ^= 1
	if _, err := Decode(flipped, cat); !errors.Is(err, ErrCorrupt) {
		t.Errorf("flipped byte: %v", err)
	}
	if _, err := Decode(good[:len(good)-9], cat); err == nil {
		t.Error("accepted a truncated file")
	}
	if _, err := Decode([]byte("not a table"), cat); err == nil {
		t.Error("accepted junk")
	}
	other := schema.New(append(append([]schema.Field{}, schema.RawFields...), schema.RiskScoreField))
	if _, err := Decode(good, other); err == nil || !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("accepted a table for another catalog: %v", err)
	}
	renamed := schema.New(append([]schema.Field{{Name: "amount_usd", Kind: schema.Number}}, cat.Fields()[1:]...))
	if _, err := Decode(good, renamed); err == nil || !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("accepted a renamed field: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing"), cat); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestValidate(t *testing.T) {
	cat := schema.Default()
	mk := func() *Table { return Synthetic(cat, SynthOptions{Rows: 10, Seed: 1}) }
	for name, breakIt := range map[string]func(*Table){
		"short column":   func(t *Table) { t.Num[0] = t.Num[0][:5] },
		"short meta":     func(t *Table) { t.DT = t.DT[:5] },
		"code overflow":  func(t *Table) { t.Str[0][3] = 999 },
		"dict not empty": func(t *Table) { t.Dict[0][0] = "x" },
		"second empty":   func(t *Table) { t.Dict[0] = append(t.Dict[0], "") },
		"bad label":      func(t *Table) { t.Fraud[2] = 7 },
		"label times":    func(t *Table) { t.LabelTime = make([]int64, 3) },
		"columns":        func(t *Table) { t.Str = t.Str[:1] },
		"no catalog":     func(t *Table) { t.Catalog = nil },
		"short str":      func(t *Table) { t.Str[1] = t.Str[1][:2] },
	} {
		tbl := mk()
		breakIt(tbl)
		if tbl.Validate() == nil {
			t.Errorf("%s: Validate passed", name)
		}
		if tbl.Encode(&bytes.Buffer{}) == nil {
			t.Errorf("%s: Encode wrote an invalid table", name)
		}
	}
	if err := mk().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderPanicsOnForeignRow(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	b := NewBuilder(schema.Default(), 1)
	b.Append(schema.Row{Num: []float64{1}}, Meta{})
}

func TestRowsInTime(t *testing.T) {
	tbl := Synthetic(schema.Default(), SynthOptions{Rows: 1000, Seed: 2, Days: 10})
	lo, hi := tbl.TimeSpan()
	if lo != 86400 || hi >= 11*86400 {
		t.Fatalf("span %d..%d", lo, hi)
	}
	in := tbl.RowsInTime(2*86400, 3*86400)
	for i, dt := range tbl.DT {
		if in.Get(i) != (dt >= 2*86400 && dt < 3*86400) {
			t.Fatalf("row %d", i)
		}
	}
	if n := in.Count(); n < 80 || n > 120 {
		t.Fatalf("%d rows in one of ten days", n)
	}
	var empty Table
	if a, b := empty.TimeSpan(); a != 0 || b != 0 {
		t.Fatal("empty span")
	}
}

// TestEvalRowsAndWorkers checks the parallel and row-range paths against
// the single-threaded whole-table result.
func TestEvalRowsAndWorkers(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 3*chunkRows + 77, Seed: 5})
	r := mustRule(t, `block if :risk_score: >= 50 or lower(:purchaser_email_domain:) = "gmail.com"`)
	p, err := CompileVector(r.Rule.Cond, tbl)
	if err != nil {
		t.Fatal(err)
	}
	one := p.EvalWorkers(1)
	if !one.Equal(p.Eval()) || !one.Equal(p.EvalWorkers(7)) {
		t.Fatal("parallel evaluation differs from single-threaded")
	}
	for _, rg := range [][2]int{{0, tbl.N}, {5, 70}, {64, 128}, {100, chunkRows + 3}, {chunkRows - 1, 2*chunkRows + 1}, {tbl.N - 3, tbl.N + 50}, {50, 50}, {-5, 3}} {
		got := p.EvalRows(rg[0], rg[1], 3)
		for i := range tbl.N {
			want := one.Get(i) && i >= rg[0] && i < rg[1]
			if got.Get(i) != want {
				t.Fatalf("range %v: row %d = %v", rg, i, got.Get(i))
			}
		}
	}
	// Programs are bound to their table.
	other := Synthetic(env.Catalog, SynthOptions{Rows: 10, Seed: 5})
	defer func() {
		if recover() == nil {
			t.Error("evaluated a program over another table")
		}
	}()
	EvalMany(other, []*Program{p}, 0, 10, 1)
}

func TestMatchTableAndBuilderLen(t *testing.T) {
	env := testEnv()
	tbl := Synthetic(env.Catalog, SynthOptions{Rows: 500, Seed: 3})
	r := mustRule(t, `block if lower(:purchaser_email_domain:) = "gmail.com" or :risk_score: > 60`)
	p, err := CompileVector(r.Rule.Cond, tbl)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchTable(r, tbl).Equal(p.Eval()) || !MatchRows(r, tbl.Rows()).Equal(p.Eval()) {
		t.Fatal("row-at-a-time paths disagree with the vectorized one")
	}
	b := NewBuilder(env.Catalog, 0)
	b.Append(env.Catalog.NewRow(), Meta{})
	if b.Len() != 1 {
		t.Fatal("Len")
	}
	if err := tbl.Save(filepath.Join(t.TempDir(), "no", "such", "dir", "t.rgt")); err == nil {
		t.Fatal("saved into a missing directory")
	}
}
