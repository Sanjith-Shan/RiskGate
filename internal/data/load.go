package data

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// IEEE-CIS column names. The transaction file has about 394 columns, and
// RiskGate reads the handful below by header name, so column order and the
// columns it ignores do not matter.
const (
	ColTransactionID  = "TransactionID"
	ColIsFraud        = "isFraud"
	ColTransactionDT  = "TransactionDT"
	ColTransactionAmt = "TransactionAmt"
	ColProductCD      = "ProductCD"
	ColCard1          = "card1"
	ColCard4          = "card4"
	ColCard6          = "card6"
	ColAddr1          = "addr1"
	ColAddr2          = "addr2"
	ColDist1          = "dist1"
	ColPEmail         = "P_emaildomain"
	ColREmail         = "R_emaildomain"
	ColD1             = "D1"
	ColDeviceType     = "DeviceType"
	ColDeviceInfo     = "DeviceInfo"
)

// Identity is the part of an identity-table row that RiskGate uses.
// Only about a quarter of IEEE-CIS transactions have an identity row.
type Identity struct {
	DeviceType string
	DeviceInfo string
}

// transaction-file columns, in the order parseTxn reads them.
var txnColumns = []string{
	ColTransactionID, ColTransactionDT, ColTransactionAmt, ColProductCD,
	ColCard1, ColCard4, ColCard6, ColAddr1, ColAddr2, ColDist1,
	ColPEmail, ColREmail, ColD1,
}

const (
	iID = iota
	iDT
	iAmt
	iProduct
	iCard1
	iCard4
	iCard6
	iAddr1
	iAddr2
	iDist1
	iPEmail
	iREmail
	iD1
)

// ReadTransactions streams an IEEE-CIS transaction CSV. isFraud is optional
// (the Kaggle test file has none) and becomes -1 when absent. Every other
// column in txnColumns is required. The result is in file order; call Sort.
func ReadTransactions(r io.Reader) ([]Txn, error) {
	cr := newCSVReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("transactions: reading header: %w", err)
	}
	idx, err := columnIndex(header, txnColumns)
	if err != nil {
		return nil, fmt.Errorf("transactions: %w", err)
	}
	fraudCol := indexOf(header, ColIsFraud)

	strs := make(interner)
	var out []Txn
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("transactions: %w", err)
		}
		t, err := parseTxn(rec, idx, fraudCol, strs)
		if err != nil {
			line, _ := cr.FieldPos(0)
			return nil, fmt.Errorf("transactions: line %d: %w", line, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func parseTxn(rec []string, idx []int, fraudCol int, strs interner) (Txn, error) {
	var t Txn
	var err error
	if t.ID, err = parseInt(ColTransactionID, rec[idx[iID]]); err != nil {
		return t, err
	}
	if t.DT, err = parseInt(ColTransactionDT, rec[idx[iDT]]); err != nil {
		return t, err
	}
	nums := [...]struct {
		col string
		dst *float64
		i   int
	}{
		{ColTransactionAmt, &t.Amount, iAmt},
		{ColCard1, &t.Card1, iCard1},
		{ColAddr1, &t.Addr1, iAddr1},
		{ColAddr2, &t.Addr2, iAddr2},
		{ColDist1, &t.Dist1, iDist1},
		{ColD1, &t.D1, iD1},
	}
	for _, n := range nums {
		if *n.dst, err = parseFloat(n.col, rec[idx[n.i]]); err != nil {
			return t, err
		}
	}
	t.ProductCode = strs.get(rec[idx[iProduct]])
	t.Card4 = strs.get(rec[idx[iCard4]])
	t.Card6 = strs.get(rec[idx[iCard6]])
	t.PEmail = strs.get(rec[idx[iPEmail]])
	t.REmail = strs.get(rec[idx[iREmail]])
	t.IsFraud = -1
	if fraudCol >= 0 {
		switch rec[fraudCol] {
		case "0":
			t.IsFraud = 0
		case "1":
			t.IsFraud = 1
		case "":
		default:
			return t, fmt.Errorf("%s: want 0 or 1, got %q", ColIsFraud, rec[fraudCol])
		}
	}
	return t, nil
}

// ReadIdentity streams an IEEE-CIS identity CSV into a map keyed by
// TransactionID.
func ReadIdentity(r io.Reader) (map[int64]Identity, error) {
	cr := newCSVReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("identity: reading header: %w", err)
	}
	idx, err := columnIndex(header, []string{ColTransactionID, ColDeviceType, ColDeviceInfo})
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	strs := make(interner)
	out := make(map[int64]Identity)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("identity: %w", err)
		}
		id, err := parseInt(ColTransactionID, rec[idx[0]])
		if err != nil {
			line, _ := cr.FieldPos(0)
			return nil, fmt.Errorf("identity: line %d: %w", line, err)
		}
		if _, dup := out[id]; dup {
			line, _ := cr.FieldPos(0)
			return nil, fmt.Errorf("identity: line %d: duplicate %s %d", line, ColTransactionID, id)
		}
		out[id] = Identity{DeviceType: strs.get(rec[idx[1]]), DeviceInfo: strs.get(rec[idx[2]])}
	}
	return out, nil
}

// Join copies identity fields onto the transactions that have an identity
// row. Identity rows without a matching transaction are ignored, as in the
// competition's left join.
func Join(txns []Txn, ids map[int64]Identity) {
	for i := range txns {
		if id, ok := ids[txns[i].ID]; ok {
			txns[i].DeviceType = id.DeviceType
			txns[i].DeviceInfo = id.DeviceInfo
		}
	}
}

// Sort orders transactions by Less: event time, then TransactionID.
func Sort(txns []Txn) {
	sort.Slice(txns, func(i, j int) bool { return Less(&txns[i], &txns[j]) })
}

// CheckSorted reports the first position where txns is not strictly
// increasing under Less. A repeated (DT, TransactionID) pair is an error too:
// it would make the replay order, and therefore the features, ambiguous.
func CheckSorted(txns []Txn) error {
	for i := 1; i < len(txns); i++ {
		if !Less(&txns[i-1], &txns[i]) {
			return fmt.Errorf("transactions not strictly ordered at index %d: (DT %d, ID %d) then (DT %d, ID %d)",
				i, txns[i-1].DT, txns[i-1].ID, txns[i].DT, txns[i].ID)
		}
	}
	return nil
}

// ReadCSV reads, joins, sorts, and checks a transaction file and an optional
// identity file (pass "" to skip identity).
func ReadCSV(transactionsPath, identityPath string) ([]Txn, error) {
	txns, err := readFile(transactionsPath, ReadTransactions)
	if err != nil {
		return nil, err
	}
	if identityPath != "" {
		ids, err := readFile(identityPath, ReadIdentity)
		if err != nil {
			return nil, err
		}
		Join(txns, ids)
	}
	Sort(txns)
	if err := CheckSorted(txns); err != nil {
		return nil, fmt.Errorf("%s: %w", transactionsPath, err)
	}
	return txns, nil
}

func readFile[T any](path string, read func(io.Reader) (T, error)) (T, error) {
	f, err := os.Open(path)
	if err != nil {
		var zero T
		return zero, err
	}
	defer f.Close()
	v, err := read(f)
	if err != nil {
		return v, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// Paths locates a dataset on disk.
type Paths struct {
	Transactions string // train_transaction.csv
	Identity     string // train_identity.csv; "" if there is none
	Cache        string // binary cache; "" disables caching
}

// RealPaths is where scripts/fetch_data.sh puts the IEEE-CIS files.
func RealPaths(dataDir string) Paths {
	return Paths{
		Transactions: filepath.Join(dataDir, "raw", "train_transaction.csv"),
		Identity:     filepath.Join(dataDir, "raw", "train_identity.csv"),
		Cache:        filepath.Join(dataDir, "cache", "ieee.rgc"),
	}
}

// SyntheticPaths is where cmd/synth writes its IEEE-CIS-shaped files.
func SyntheticPaths(dataDir string) Paths {
	return Paths{
		Transactions: filepath.Join(dataDir, "synth", "train_transaction.csv"),
		Identity:     filepath.Join(dataDir, "synth", "train_identity.csv"),
		Cache:        filepath.Join(dataDir, "cache", "synth.rgc"),
	}
}

// SyntheticMarker is the file cmd/synth writes next to its CSVs. Its presence
// marks the dataset synthetic, and every tool that reads it must say so.
const SyntheticMarker = "SYNTHETIC"

// Dataset is a loaded, sorted transaction table.
type Dataset struct {
	Txns      []Txn // sorted by Less
	Calendar  Calendar
	Synthetic bool // generated by cmd/synth, not IEEE-CIS
	FromCache bool
}

// Load returns the dataset at p, using the binary cache when it is still
// fresh for the CSVs (same sizes and modification times) and rebuilding it
// otherwise. If the CSVs are gone but a cache exists, the cache is used.
func Load(p Paths) (*Dataset, error) {
	fp, srcErr := fingerprint(p)
	if p.Cache != "" {
		txns, meta, err := ReadCacheFile(p.Cache)
		switch {
		case err == nil && (srcErr != nil || meta.Fingerprint == fp):
			return newDataset(txns, meta.Synthetic, true), nil
		case err != nil && !errors.Is(err, os.ErrNotExist) && srcErr != nil:
			return nil, err
		}
	}
	if srcErr != nil {
		return nil, srcErr
	}
	txns, err := ReadCSV(p.Transactions, p.Identity)
	if err != nil {
		return nil, err
	}
	synthetic := isSynthetic(p.Transactions)
	if p.Cache != "" {
		if err := WriteCacheFile(p.Cache, txns, CacheMeta{Fingerprint: fp, Synthetic: synthetic}); err != nil {
			return nil, err
		}
	}
	return newDataset(txns, synthetic, false), nil
}

func newDataset(txns []Txn, synthetic, cached bool) *Dataset {
	return &Dataset{Txns: txns, Calendar: CalendarFor(txns), Synthetic: synthetic, FromCache: cached}
}

func isSynthetic(transactionsPath string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(transactionsPath), SyntheticMarker))
	return err == nil
}

// fingerprint identifies the source files by size and modification time,
// which is enough to notice a re-download or a regenerated synthetic set.
func fingerprint(p Paths) (string, error) {
	var b strings.Builder
	b.WriteString("v1")
	for _, path := range []string{p.Transactions, p.Identity} {
		if path == "" {
			b.WriteString("|-")
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "|%d:%d", st.Size(), st.ModTime().UnixNano())
	}
	if isSynthetic(p.Transactions) {
		b.WriteString("|synthetic")
	}
	return b.String(), nil
}

func newCSVReader(r io.Reader) *csv.Reader {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.ReuseRecord = true
	return cr
}

func indexOf(header []string, name string) int {
	for i, h := range header {
		if strings.TrimSpace(h) == name {
			return i
		}
	}
	return -1
}

func columnIndex(header []string, names []string) ([]int, error) {
	idx := make([]int, len(names))
	var missing []string
	for i, n := range names {
		if idx[i] = indexOf(header, n); idx[i] < 0 {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing column(s) %s", strings.Join(missing, ", "))
	}
	return idx, nil
}

// parseFloat reads a numeric cell. Empty means missing (NaN).
func parseFloat(col, s string) (float64, error) {
	if s == "" {
		return math.NaN(), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: bad number %q", col, s)
	}
	return v, nil
}

// parseInt reads a required integer cell. It accepts "86400.0", which is how
// pandas writes an integer column that ever held a NaN.
func parseInt(col, s string) (int64, error) {
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, fmt.Errorf("%s: want an integer, got %q", col, s)
	}
	return int64(f), nil
}

// interner deduplicates categorical strings. It matters for memory, not just
// speed: encoding/csv backs every field of a record with one string per line,
// so keeping a field would pin the whole ~3 KB IEEE-CIS line in memory.
type interner map[string]string

func (in interner) get(s string) string {
	if s == "" {
		return ""
	}
	if v, ok := in[s]; ok {
		return v
	}
	c := strings.Clone(s)
	in[c] = c
	return c
}
