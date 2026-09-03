package data

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ReplayEpochUnix is the wall-clock second assigned to TransactionDT = 0 when
// IEEE-CIS is replayed through Clearinghouse: 2017-12-01T00:00:00Z. The date is
// the one the competition's community commonly assumes for the reference
// time; nothing depends on it being right, only on it being fixed.
const ReplayEpochUnix int64 = 1512086400

// Keys of the risk_fields object sent to POST /v1/assess. They keep the
// IEEE-CIS column names (product_code aside) so a payload can be checked
// against the dataset by eye.
const (
	RFProductCode    = "product_code"
	RFCard1          = "card1"
	RFCard4          = "card4"
	RFCard6          = "card6"
	RFAddr1          = "addr1"
	RFAddr2          = "addr2"
	RFDist1          = "dist1"
	RFPEmail         = "P_emaildomain"
	RFREmail         = "R_emaildomain"
	RFDeviceType     = "DeviceType"
	RFDeviceInfo     = "DeviceInfo"
	RFD1             = "D1"
	RFTransactionAmt = "TransactionAmt"
)

// ReplayEvent is one line of the test-month replay file that drives
// Clearinghouse, and the shape of what Clearinghouse forwards to RiskGate.
type ReplayEvent struct {
	PaymentID  string         `json:"payment_id"` // "txn_<TransactionID>"
	Created    int64          `json:"created"`    // ReplayEpochUnix + TransactionDT
	Amount     int64          `json:"amount"`     // cents, rounded half away from zero
	Currency   string         `json:"currency"`   // always "usd"
	RiskFields map[string]any `json:"risk_fields"`
	IsFraud    int            `json:"is_fraud"`
}

// PaymentIDPrefix prefixes the TransactionID in a payment id.
const PaymentIDPrefix = "txn_"

// NewReplayEvent builds the replay line for t.
func NewReplayEvent(t *Txn) ReplayEvent {
	return ReplayEvent{
		PaymentID:  PaymentIDPrefix + strconv.FormatInt(t.ID, 10),
		Created:    ReplayEpochUnix + t.DT,
		Amount:     int64(math.Round(t.Amount * 100)),
		Currency:   "usd",
		RiskFields: RiskFields(t),
		IsFraud:    int(t.IsFraud),
	}
}

// Txn recovers the transaction from a replay line. When risk_fields carries
// TransactionAmt, that exact dollar amount wins over the rounded cents.
func (e ReplayEvent) Txn() (Txn, error) {
	t, err := ParseRiskFields(e.RiskFields)
	if err != nil {
		return t, err
	}
	idStr, ok := strings.CutPrefix(e.PaymentID, PaymentIDPrefix)
	if !ok {
		return t, fmt.Errorf("payment_id %q: want %s<TransactionID>", e.PaymentID, PaymentIDPrefix)
	}
	if t.ID, err = strconv.ParseInt(idStr, 10, 64); err != nil {
		return t, fmt.Errorf("payment_id %q: %w", e.PaymentID, err)
	}
	t.DT = DTFromUnix(e.Created)
	if _, ok := e.RiskFields[RFTransactionAmt]; !ok {
		t.Amount = float64(e.Amount) / 100
	}
	switch e.IsFraud {
	case 0, 1:
		t.IsFraud = int8(e.IsFraud)
	default:
		t.IsFraud = -1
	}
	return t, nil
}

// DTFromUnix converts a replayed payment's created time back to TransactionDT.
func DTFromUnix(sec int64) int64 { return sec - ReplayEpochUnix }

// riskField describes one key: how to read it from and write it to a Txn.
type riskField struct {
	key string
	num func(*Txn) *float64
	str func(*Txn) *string
}

var riskFieldTable = []riskField{
	{key: RFProductCode, str: func(t *Txn) *string { return &t.ProductCode }},
	{key: RFCard1, num: func(t *Txn) *float64 { return &t.Card1 }},
	{key: RFCard4, str: func(t *Txn) *string { return &t.Card4 }},
	{key: RFCard6, str: func(t *Txn) *string { return &t.Card6 }},
	{key: RFAddr1, num: func(t *Txn) *float64 { return &t.Addr1 }},
	{key: RFAddr2, num: func(t *Txn) *float64 { return &t.Addr2 }},
	{key: RFDist1, num: func(t *Txn) *float64 { return &t.Dist1 }},
	{key: RFPEmail, str: func(t *Txn) *string { return &t.PEmail }},
	{key: RFREmail, str: func(t *Txn) *string { return &t.REmail }},
	{key: RFDeviceType, str: func(t *Txn) *string { return &t.DeviceType }},
	{key: RFDeviceInfo, str: func(t *Txn) *string { return &t.DeviceInfo }},
	{key: RFD1, num: func(t *Txn) *float64 { return &t.D1 }},
	{key: RFTransactionAmt, num: func(t *Txn) *float64 { return &t.Amount }},
}

var riskFieldByKey = func() map[string]*riskField {
	m := make(map[string]*riskField, len(riskFieldTable))
	for i := range riskFieldTable {
		m[riskFieldTable[i].key] = &riskFieldTable[i]
	}
	return m
}()

// RiskFieldKeys lists every accepted risk_fields key, sorted.
func RiskFieldKeys() []string {
	out := make([]string, len(riskFieldTable))
	for i, f := range riskFieldTable {
		out[i] = f.key
	}
	sort.Strings(out)
	return out
}

// RiskFields renders t's raw fields for the wire. Missing values are left
// out entirely rather than sent as null, since JSON has no NaN.
func RiskFields(t *Txn) map[string]any {
	m := make(map[string]any, len(riskFieldTable))
	for _, f := range riskFieldTable {
		if f.num != nil {
			if v := *f.num(t); !math.IsNaN(v) {
				m[f.key] = v
			}
		} else if v := *f.str(t); v != "" {
			m[f.key] = v
		}
	}
	return m
}

// ParseRiskFields builds a Txn from a decoded risk_fields object. Absent
// keys, JSON null, and "" are missing. The ID, DT, and label are left zero,
// zero, and -1 (unknown) for the caller to fill in.
//
// Unknown keys and wrongly typed values are errors, not ignored: a typo such
// as "card_1" would otherwise turn silently into a missing value online while
// the offline export had the real one, which is exactly the train/serve skew
// RiskGate exists to rule out. Numbers may arrive as float64 (the default
// encoding/json decoding), json.Number (Decoder.UseNumber), int, or int64.
func ParseRiskFields(m map[string]any) (Txn, error) {
	t := Txn{
		Amount: NaN, Card1: NaN, Addr1: NaN, Addr2: NaN, Dist1: NaN, D1: NaN,
		IsFraud: -1,
	}
	var unknown []string
	for k, v := range m {
		f, ok := riskFieldByKey[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		if v == nil {
			continue
		}
		if f.str != nil {
			s, ok := v.(string)
			if !ok {
				return t, fmt.Errorf("risk_fields.%s: want a string, got %T", k, v)
			}
			*f.str(&t) = s
			continue
		}
		x, err := toFloat(v)
		if err != nil {
			return t, fmt.Errorf("risk_fields.%s: %w", k, err)
		}
		*f.num(&t) = x
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return t, fmt.Errorf("risk_fields: unknown key(s) %s (accepted: %s)",
			strings.Join(unknown, ", "), strings.Join(RiskFieldKeys(), ", "))
	}
	return t, nil
}

func toFloat(v any) (float64, error) {
	var x float64
	switch n := v.(type) {
	case float64:
		x = n
	case json.Number:
		f, err := strconv.ParseFloat(string(n), 64)
		if err != nil {
			return 0, fmt.Errorf("bad number %q", string(n))
		}
		x = f
	case int:
		x = float64(n)
	case int64:
		x = float64(n)
	default:
		return 0, fmt.Errorf("want a number, got %T", v)
	}
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return 0, fmt.Errorf("want a finite number, got %v", x)
	}
	return x, nil
}
