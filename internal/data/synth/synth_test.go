package synth

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

func same(a, b data.Txn) bool {
	f := func(x, y float64) bool { return math.Float64bits(x) == math.Float64bits(y) }
	return a.ID == b.ID && a.DT == b.DT && f(a.Amount, b.Amount) && a.ProductCode == b.ProductCode &&
		f(a.Card1, b.Card1) && a.Card4 == b.Card4 && a.Card6 == b.Card6 && f(a.Addr1, b.Addr1) &&
		f(a.Addr2, b.Addr2) && f(a.Dist1, b.Dist1) && a.PEmail == b.PEmail && a.REmail == b.REmail &&
		a.DeviceType == b.DeviceType && a.DeviceInfo == b.DeviceInfo && f(a.D1, b.D1) && a.IsFraud == b.IsFraud
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, _ := Generate(Config{Rows: 5000, Seed: 7})
	b, _ := Generate(Config{Rows: 5000, Seed: 7})
	c, _ := Generate(Config{Rows: 5000, Seed: 8})
	differ := false
	for i := range a {
		if !same(a[i], b[i]) {
			t.Fatalf("row %d differs between runs with one seed", i)
		}
		differ = differ || !same(a[i], c[i])
	}
	if !differ {
		t.Error("different seeds gave identical data")
	}
}

func TestGenerateShape(t *testing.T) {
	txns, s := Generate(Config{Rows: 50000, Seed: 1})
	if len(txns) != 50000 || s.Rows != 50000 {
		t.Fatalf("got %d rows", len(txns))
	}
	if err := data.CheckSorted(txns); err != nil {
		t.Fatal(err)
	}
	fraud := 0
	for i := range txns {
		tx := &txns[i]
		if tx.ID != FirstID+int64(i) {
			t.Fatalf("row %d has ID %d", i, tx.ID)
		}
		if tx.DT < startDT || tx.DT >= startDT+182*data.SecondsPerDay {
			t.Fatalf("DT %d out of range", tx.DT)
		}
		if tx.Amount <= 0 || tx.Amount != math.Round(tx.Amount*1000)/1000 {
			t.Fatalf("amount %v is not positive with at most three decimals", tx.Amount)
		}
		if !math.IsNaN(tx.D1) && (tx.D1 < 0 || tx.D1 > 640) {
			t.Fatalf("D1 %v out of range", tx.D1)
		}
		fraud += int(tx.IsFraud)
	}
	if fraud != s.Fraud {
		t.Errorf("summary says %d fraud, rows say %d", s.Fraud, fraud)
	}
	if rate := float64(fraud) / float64(len(txns)); math.Abs(rate-0.035) > 0.002 {
		t.Errorf("fraud rate %.4f, want about 0.035", rate)
	}
	for _, p := range []string{PatternCardTesting, PatternAccountTakeover, PatternRawFields} {
		if s.ByPattern[p] == 0 {
			t.Errorf("no %s fraud", p)
		}
	}
	if id := float64(s.WithIdentity) / float64(len(txns)); id < 0.15 || id > 0.45 {
		t.Errorf("identity share %.2f, want roughly a quarter", id)
	}
}

// The uid of a legitimate customer must stay constant across their
// payments: day - D1 is the card's first-use day, unless D1 was clipped.
func TestUIDIsStableForACustomer(t *testing.T) {
	txns, _ := Generate(Config{Rows: 20000, Seed: 3})
	type card struct{ card1, addr1 float64 }
	anchors := map[card]map[float64]int{}
	for _, tx := range txns {
		if tx.IsFraud == 1 || math.IsNaN(tx.D1) || math.IsNaN(tx.Addr1) || tx.D1 == 640 {
			continue
		}
		k := card{tx.Card1, tx.Addr1}
		if anchors[k] == nil {
			anchors[k] = map[float64]int{}
		}
		anchors[k][float64(tx.DT/data.SecondsPerDay)-tx.D1]++
	}
	repeat := 0
	for _, m := range anchors {
		for _, n := range m {
			if n > 1 {
				repeat++
			}
		}
	}
	if repeat < 500 {
		t.Errorf("only %d uids recur; the uid construction is not being exercised", repeat)
	}
}

func TestWriteCSVRoundTrips(t *testing.T) {
	txns, _ := Generate(Config{Rows: 3000, Seed: 5})
	dir := t.TempDir()
	if err := WriteCSV(dir, txns); err != nil {
		t.Fatal(err)
	}
	d, err := data.Load(data.Paths{
		Transactions: filepath.Join(dir, "train_transaction.csv"),
		Identity:     filepath.Join(dir, "train_identity.csv"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Synthetic {
		t.Error("written data not marked synthetic")
	}
	if len(d.Txns) != len(txns) {
		t.Fatalf("read %d rows, wrote %d", len(d.Txns), len(txns))
	}
	for i := range txns {
		if !same(d.Txns[i], txns[i]) {
			t.Fatalf("row %d:\n got %+v\nwant %+v", i, d.Txns[i], txns[i])
		}
	}
}
