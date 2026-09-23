package data

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestReplayEpoch(t *testing.T) {
	want := time.Date(2017, 12, 1, 0, 0, 0, 0, time.UTC).Unix()
	if ReplayEpochUnix != want {
		t.Errorf("ReplayEpochUnix = %d, want %d", ReplayEpochUnix, want)
	}
}

func TestNewReplayEvent(t *testing.T) {
	m := byID(loadFixture(t))
	tx := m[100008] // $0.251, several fields missing
	e := NewReplayEvent(&tx)
	if e.PaymentID != "txn_100008" || e.Created != ReplayEpochUnix+13046400 || e.Amount != 25 || e.Currency != "usd" {
		t.Errorf("got %+v", e)
	}
	for _, k := range []string{RFAddr1, RFAddr2, RFDist1, RFD1, RFDeviceType, RFDeviceInfo} {
		if _, ok := e.RiskFields[k]; ok {
			t.Errorf("missing field %s was sent", k)
		}
	}
	if e.RiskFields[RFTransactionAmt] != 0.251 {
		t.Errorf("TransactionAmt: got %v", e.RiskFields[RFTransactionAmt])
	}
}

// roundTrip sends t through the JSON wire format and back.
func roundTrip(t *testing.T, tx Txn, useNumber bool) Txn {
	t.Helper()
	b, err := json.Marshal(NewReplayEvent(&tx))
	if err != nil {
		t.Fatal(err)
	}
	var e ReplayEvent
	dec := json.NewDecoder(bytes.NewReader(b))
	if useNumber {
		dec.UseNumber()
	}
	if err := dec.Decode(&e); err != nil {
		t.Fatal(err)
	}
	got, err := e.Txn()
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return got
}

func TestReplayEventRoundTripIsExact(t *testing.T) {
	txns := loadFixture(t)
	r := rand.New(rand.NewPCG(1, 2))
	maybeNaN := func(x float64) float64 {
		if r.IntN(4) == 0 {
			return math.NaN()
		}
		return x
	}
	maybeEmpty := func(s string) string {
		if r.IntN(4) == 0 {
			return ""
		}
		return s
	}
	for i := range 2000 {
		txns = append(txns, Txn{
			ID:     int64(3_000_000 + i),
			DT:     r.Int64N(16_000_000),
			Amount: r.Float64() * 5000, // arbitrary bit patterns, not just cents
			// Negative zero is deliberately absent: JSON cannot carry its sign.
			ProductCode: maybeEmpty("W"),
			Card1:       maybeNaN(float64(1000 + r.IntN(17000))),
			Card4:       maybeEmpty("visa"),
			Card6:       maybeEmpty("debit"),
			Addr1:       maybeNaN(float64(100 + r.IntN(440))),
			Addr2:       maybeNaN(87),
			Dist1:       maybeNaN(r.ExpFloat64() * 100),
			PEmail:      maybeEmpty("gmail.com"),
			REmail:      maybeEmpty("anonymous.com"),
			DeviceType:  maybeEmpty("mobile"),
			DeviceInfo:  maybeEmpty(`Test "Device", with \ escapes ✓`),
			D1:          maybeNaN(float64(r.IntN(641))),
			IsFraud:     int8(r.IntN(2)),
		})
	}
	for _, useNumber := range []bool{false, true} {
		for _, tx := range txns {
			if got := roundTrip(t, tx, useNumber); !sameTxn(got, tx) {
				t.Fatalf("UseNumber=%v:\n got %+v\nwant %+v", useNumber, got, tx)
			}
		}
	}
}

func TestParseRiskFields(t *testing.T) {
	tx, err := ParseRiskFields(map[string]any{
		"card1": int64(5001), "addr1": 210, "D1": nil, "P_emaildomain": "",
		"product_code": "W", "TransactionAmt": json.Number("41.25"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Card1 != 5001 || tx.Addr1 != 210 || !math.IsNaN(tx.D1) || tx.PEmail != "" ||
		tx.ProductCode != "W" || tx.Amount != 41.25 || !math.IsNaN(tx.Dist1) || tx.IsFraud != -1 {
		t.Errorf("got %+v", tx)
	}
}

func TestParseRiskFieldsErrors(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]any
		want string
	}{
		{"unknown key", map[string]any{"card_1": 5.0, "zeta": 1.0}, "unknown key(s) card_1, zeta"},
		{"number as string", map[string]any{"card1": "5001"}, "risk_fields.card1: want a number, got string"},
		{"string as number", map[string]any{"card4": 3.0}, "risk_fields.card4: want a string, got float64"},
		{"bad json number", map[string]any{"dist1": json.Number("1e999")}, "risk_fields.dist1"},
		{"bool", map[string]any{"D1": true}, "want a number, got bool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRiskFields(tt.m)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestReplayEventTxnErrors(t *testing.T) {
	for _, id := range []string{"100001", "txn_", "txn_12x", "ch_1"} {
		if _, err := (ReplayEvent{PaymentID: id}).Txn(); err == nil {
			t.Errorf("payment_id %q accepted", id)
		}
	}
	if _, err := (ReplayEvent{PaymentID: "txn_1", RiskFields: map[string]any{"x": 1.0}}).Txn(); err == nil {
		t.Error("unknown risk field accepted")
	}
}

func TestReplayEventAmountFallsBackToCents(t *testing.T) {
	tx, err := ReplayEvent{PaymentID: "txn_5", Created: ReplayEpochUnix + 100, Amount: 1999, IsFraud: 7}.Txn()
	if err != nil {
		t.Fatal(err)
	}
	if tx.Amount != 19.99 || tx.ID != 5 || tx.DT != 100 || tx.IsFraud != -1 {
		t.Errorf("got %+v", tx)
	}
}
