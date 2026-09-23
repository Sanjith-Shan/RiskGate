// Package data loads IEEE-CIS transactions, orders them in event time, and
// splits them into months.
package data

import "math"

// Txn is one payment with the raw IEEE-CIS columns RiskGate uses. Missing
// numbers are NaN; missing strings are "".
type Txn struct {
	ID          int64   // TransactionID
	DT          int64   // TransactionDT, seconds from the dataset's reference time
	Amount      float64 // TransactionAmt, dollars
	ProductCode string  // ProductCD
	Card1       float64 // anonymized card id; entity key "card"
	Card4       string  // network
	Card6       string  // type
	Addr1       float64
	Addr2       float64
	Dist1       float64
	PEmail      string // P_emaildomain
	REmail      string // R_emaildomain
	DeviceType  string // from the identity table
	DeviceInfo  string // from the identity table
	D1          float64
	IsFraud     int8 // 1 fraud, 0 legitimate, -1 unknown
}

// Less orders transactions in event time, ties broken by TransactionID. This
// is the one ordering used by the replay driver, the export, and the split.
func Less(a, b *Txn) bool {
	if a.DT != b.DT {
		return a.DT < b.DT
	}
	return a.ID < b.ID
}

// NaN is the missing number.
var NaN = math.NaN()
