package data

import "fmt"

// Time constants for TransactionDT, which counts seconds from an undisclosed
// reference instant.
const (
	SecondsPerDay   = 86400
	SecondsPerMonth = 30 * SecondsPerDay

	// NumMonths is the number of months in the split: four train, one
	// validation, one test.
	NumMonths = 6
)

// Split is the role a month plays in evaluation.
type Split int8

const (
	Train Split = iota
	Valid
	Test
)

func (s Split) String() string {
	switch s {
	case Train:
		return "train"
	case Valid:
		return "valid"
	case Test:
		return "test"
	}
	return fmt.Sprintf("Split(%d)", int8(s))
}

// SplitOfMonth maps a month index to its split: months 0-3 train, 4
// validation, 5 test.
func SplitOfMonth(m int) Split {
	switch {
	case m <= 3:
		return Train
	case m == 4:
		return Valid
	}
	return Test
}

// Calendar turns TransactionDT into month indexes.
//
// The rule: Origin is the start of the day holding the earliest transaction,
// floor(min DT / 86400) * 86400, and
//
//	month(DT) = floor((DT - Origin) / (30 * 86400))
//
// so months are 30-day blocks aligned to midnight of the reference clock.
// IEEE-CIS starts at DT = 86400 and ends at DT = 15,811,131, about 182 days,
// which makes six full months and a seventh of about two days. That partial
// month is folded into the test month (index 5) rather than dropped, because
// it is labelled data contiguous with the test month and dropping it would
// waste the most recent fraud. Months before the origin clamp to 0.
type Calendar struct {
	Origin int64 // TransactionDT at the start of month 0
}

// CalendarFor anchors a calendar at the day of the earliest transaction.
// An empty slice gives Origin 0.
func CalendarFor(txns []Txn) Calendar {
	if len(txns) == 0 {
		return Calendar{}
	}
	lo := txns[0].DT
	for i := range txns {
		lo = min(lo, txns[i].DT)
	}
	return Calendar{Origin: floorDiv(lo, SecondsPerDay) * SecondsPerDay}
}

// Month returns the folded month index in [0, NumMonths).
func (c Calendar) Month(dt int64) int {
	m := floorDiv(dt-c.Origin, SecondsPerMonth)
	return int(max(0, min(m, NumMonths-1)))
}

// Split returns the split of the month holding dt.
func (c Calendar) Split(dt int64) Split { return SplitOfMonth(c.Month(dt)) }

// MonthStart returns the first TransactionDT of month m.
func (c Calendar) MonthStart(m int) int64 { return c.Origin + int64(m)*SecondsPerMonth }

// floorDiv divides rounding toward negative infinity, so that times before
// the origin land in negative buckets rather than bucket zero.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
