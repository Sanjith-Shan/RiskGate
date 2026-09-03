package data

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const (
	fixtureTxn = "testdata/train_transaction.csv"
	fixtureID  = "testdata/train_identity.csv"
)

// sameTxn compares every field, floats by bit pattern so that NaN == NaN and
// a rounding difference is never hidden.
func sameTxn(a, b Txn) bool {
	fa, fb := numCols(&a), numCols(&b)
	for i := range fa {
		if math.Float64bits(*fa[i]) != math.Float64bits(*fb[i]) {
			return false
		}
	}
	sa, sb := strCols(&a), strCols(&b)
	for i := range sa {
		if *sa[i] != *sb[i] {
			return false
		}
	}
	return a.ID == b.ID && a.DT == b.DT && a.IsFraud == b.IsFraud
}

func loadFixture(t *testing.T) []Txn {
	t.Helper()
	txns, err := ReadCSV(fixtureTxn, fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	return txns
}

func byID(txns []Txn) map[int64]Txn {
	m := make(map[int64]Txn, len(txns))
	for _, t := range txns {
		m[t.ID] = t
	}
	return m
}

func TestReadCSVOrdersByTimeThenID(t *testing.T) {
	txns := loadFixture(t)
	// File order is shuffled, and 100003 and 100005 share DT 86506: the
	// lower TransactionID must come first.
	want := []int64{100001, 100002, 100004, 100003, 100005, 100006, 100010, 100007, 100008, 100009}
	if len(txns) != len(want) {
		t.Fatalf("got %d rows, want %d", len(txns), len(want))
	}
	for i, id := range want {
		if txns[i].ID != id {
			t.Errorf("position %d: got ID %d, want %d", i, txns[i].ID, id)
		}
	}
	if err := CheckSorted(txns); err != nil {
		t.Error(err)
	}
}

func TestReadCSVFields(t *testing.T) {
	m := byID(loadFixture(t))
	nan := math.NaN()
	tests := []Txn{
		{ID: 100001, DT: 86400, Amount: 41.25, ProductCode: "W", Card1: 5001, Card4: "discover", Card6: "credit",
			Addr1: 210, Addr2: 87, Dist1: 12, D1: 9, IsFraud: 0},
		// Fraud, with an identity row.
		{ID: 100003, DT: 86506, Amount: 22.5, ProductCode: "W", Card1: 5003, Card4: "visa", Card6: "debit",
			Addr1: 212, Addr2: 87, Dist1: 301, PEmail: "outlook.com", D1: 0, IsFraud: 1,
			DeviceType: "mobile", DeviceInfo: "TestPhone X1 Build/TST001"},
		// Identity row with DeviceInfo missing; addr missing.
		{ID: 100006, DT: 2678500, Amount: 15.125, ProductCode: "C", Card1: 5006, Card4: "visa", Card6: "credit",
			Addr1: nan, Addr2: nan, Dist1: nan, PEmail: "anonymous.com", REmail: "anonymous.com", D1: 0, IsFraud: 1,
			DeviceType: "mobile"},
		// Three-decimal amount and missing D1.
		{ID: 100008, DT: 13046400, Amount: 0.251, ProductCode: "C", Card1: 5006, Card4: "visa", Card6: "credit",
			Addr1: nan, Addr2: nan, Dist1: nan, PEmail: "hotmail.com", REmail: "hotmail.com", D1: nan},
		// A quoted DeviceInfo containing a comma.
		{ID: 100009, DT: 15638400, Amount: 1200, ProductCode: "R", Card1: 5007, Card4: "american express", Card6: "credit",
			Addr1: 215, Addr2: 87, Dist1: nan, PEmail: "aol.com", REmail: "aol.com", D1: 1, IsFraud: 1,
			DeviceType: "desktop", DeviceInfo: "Tablet Q7, rev 2"},
		// Nearly everything missing.
		{ID: 100010, DT: 5270400, Amount: 35.95, ProductCode: "S", Card1: 5008,
			Addr1: nan, Addr2: nan, Dist1: nan, D1: nan},
	}
	for _, want := range tests {
		got, ok := m[want.ID]
		if !ok {
			t.Errorf("ID %d missing", want.ID)
			continue
		}
		if !sameTxn(got, want) {
			t.Errorf("ID %d:\n got %+v\nwant %+v", want.ID, got, want)
		}
	}
}

func TestJoinIgnoresOrphanIdentity(t *testing.T) {
	txns := loadFixture(t)
	withIdentity := 0
	for _, tx := range txns {
		if tx.DeviceType != "" {
			withIdentity++
		}
		if tx.DeviceInfo == "Orphan Device" {
			t.Errorf("orphan identity row 199999 joined onto %d", tx.ID)
		}
	}
	if withIdentity != 4 {
		t.Errorf("got %d rows with identity, want 4", withIdentity)
	}
}

func TestReadCSVWithoutIdentity(t *testing.T) {
	txns, err := ReadCSV(fixtureTxn, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range txns {
		if tx.DeviceType != "" || tx.DeviceInfo != "" {
			t.Fatalf("ID %d has identity fields without an identity file", tx.ID)
		}
	}
}

func TestReadTransactionsInternsStrings(t *testing.T) {
	m := byID(loadFixture(t))
	a, b := m[100005].PEmail, m[100002].PEmail
	if a != "gmail.com" || b != a {
		t.Fatalf("got %q and %q", a, b)
	}
	if unsafe.StringData(a) != unsafe.StringData(b) {
		t.Error("equal categorical values are not shared")
	}
}

func TestReadTransactionsErrors(t *testing.T) {
	tests := []struct {
		name, csv, want string
	}{
		{"missing column",
			"TransactionID,TransactionDT,TransactionAmt\n1,2,3\n",
			"missing column(s) ProductCD, card1"},
		{"bad number",
			header + "1,86400,12x,W,1,visa,debit,1,1,1,,,1\n",
			"line 2: TransactionAmt: bad number \"12x\""},
		{"bad id",
			header + "1.5,86400,12,W,1,visa,debit,1,1,1,,,1\n",
			"line 2: TransactionID: want an integer"},
		{"bad label",
			"isFraud," + header + "2,1,86400,12,W,1,visa,debit,1,1,1,,,1\n",
			"isFraud: want 0 or 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadTransactions(strings.NewReader(tt.csv))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got error %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

const header = "TransactionID,TransactionDT,TransactionAmt,ProductCD,card1,card4,card6,addr1,addr2,dist1,P_emaildomain,R_emaildomain,D1\n"

func TestReadTransactionsUnlabelled(t *testing.T) {
	txns, err := ReadTransactions(strings.NewReader(header + "7,86400.0,12,W,1,visa,debit,1,1,1,,,1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if txns[0].IsFraud != -1 || txns[0].DT != 86400 {
		t.Errorf("got IsFraud %d DT %d, want -1 and 86400", txns[0].IsFraud, txns[0].DT)
	}
}

func TestReadIdentityRejectsDuplicates(t *testing.T) {
	_, err := ReadIdentity(strings.NewReader("TransactionID,DeviceType,DeviceInfo\n1,mobile,a\n1,desktop,b\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("got %v, want a duplicate error", err)
	}
}

func TestCheckSortedRejectsDuplicates(t *testing.T) {
	txns := []Txn{{ID: 1, DT: 5}, {ID: 1, DT: 5}}
	if err := CheckSorted(txns); err == nil {
		t.Error("repeated (DT, ID) accepted")
	}
	if err := CheckSorted([]Txn{{ID: 2, DT: 5}, {ID: 1, DT: 5}}); err == nil {
		t.Error("tie out of ID order accepted")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	txns := loadFixture(t)
	var buf bytes.Buffer
	meta := CacheMeta{Fingerprint: "fp", Synthetic: true}
	if err := WriteCache(&buf, txns, meta); err != nil {
		t.Fatal(err)
	}
	got, gotMeta, err := ReadCache(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if gotMeta != meta {
		t.Errorf("meta: got %+v, want %+v", gotMeta, meta)
	}
	if len(got) != len(txns) {
		t.Fatalf("got %d rows, want %d", len(got), len(txns))
	}
	for i := range txns {
		if !sameTxn(got[i], txns[i]) {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, got[i], txns[i])
		}
	}
}

func TestCacheDetectsCorruption(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCache(&buf, loadFixture(t), CacheMeta{}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	for _, i := range []int{len(cacheMagic) + 3, len(b) / 2, len(b) - 1} {
		c := bytes.Clone(b)
		c[i] ^= 0x40
		if _, _, err := ReadCache(c); err == nil {
			t.Errorf("flipped byte %d went unnoticed", i)
		}
	}
	if _, _, err := ReadCache(b[:len(b)-9]); err == nil {
		t.Error("truncated cache accepted")
	}
	if _, _, err := ReadCache([]byte("not a cache at all")); err == nil {
		t.Error("garbage accepted")
	}
}

func copyFixture(t *testing.T, dir string) Paths {
	t.Helper()
	p := Paths{
		Transactions: filepath.Join(dir, "raw", "train_transaction.csv"),
		Identity:     filepath.Join(dir, "raw", "train_identity.csv"),
		Cache:        filepath.Join(dir, "cache", "ieee.rgc"),
	}
	if err := os.MkdirAll(filepath.Dir(p.Transactions), 0o755); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{fixtureTxn: p.Transactions, fixtureID: p.Identity} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestLoadUsesFreshCacheOnly(t *testing.T) {
	dir := t.TempDir()
	p := copyFixture(t, dir)
	if got := RealPaths(dir); got != p {
		t.Fatalf("RealPaths: got %+v, want %+v", got, p)
	}

	first, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if first.FromCache || first.Synthetic {
		t.Fatalf("first load: FromCache %v Synthetic %v, want false false", first.FromCache, first.Synthetic)
	}
	if first.Calendar.Origin != 86400 {
		t.Errorf("origin: got %d, want 86400", first.Calendar.Origin)
	}

	second, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FromCache {
		t.Fatal("second load did not use the cache")
	}
	for i := range first.Txns {
		if !sameTxn(first.Txns[i], second.Txns[i]) {
			t.Fatalf("row %d differs after the cache round trip", i)
		}
	}

	// A changed source invalidates the cache.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(p.Transactions, later, later); err != nil {
		t.Fatal(err)
	}
	third, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if third.FromCache {
		t.Error("stale cache was used")
	}

	// With the CSVs gone, the cache still serves.
	os.Remove(p.Transactions)
	os.Remove(p.Identity)
	fourth, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fourth.FromCache || len(fourth.Txns) != 10 {
		t.Errorf("got FromCache %v and %d rows, want the cache's 10", fourth.FromCache, len(fourth.Txns))
	}
}

func TestLoadMarksSynthetic(t *testing.T) {
	dir := t.TempDir()
	p := copyFixture(t, dir)
	if err := os.WriteFile(filepath.Join(filepath.Dir(p.Transactions), SyntheticMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 { // once from CSV, once from the cache
		d, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Synthetic {
			t.Errorf("FromCache=%v: dataset not marked synthetic", d.FromCache)
		}
	}
}

func TestLoadMissingEverything(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(Paths{Transactions: filepath.Join(dir, "nope.csv"), Cache: filepath.Join(dir, "nope.rgc")})
	if err == nil {
		t.Error("loading nothing succeeded")
	}
}
