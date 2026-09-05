package model

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Reason is one plain-English explanation of a risk score, with the Saabas
// contribution (in log-odds) of the model input it describes.
type Reason struct {
	Feature      string  `json:"feature"`
	Contribution float64 `json:"contribution"`
	Text         string  `json:"text"`
}

// Stat summarises one feature over the training months, for the "typical"
// context in a reason. Written by train.py into metadata.json.
type Stat struct {
	P50 float64 `json:"p50"`
}

// Every model input is exactly one catalog field. A categorical field is a
// single input holding its category code (LightGBM splits on the code set
// natively), so there is no one-hot family to regroup: one field, one
// contribution, at most one reason.

var entityNoun = map[string]string{
	"card":   "this card",
	"uid":    "this customer", // the approximate uid, see DESIGN.md
	"device": "this device",
	"email":  "this email domain",
}

var windowNoun = map[string]string{
	"1h":  "the last hour",
	"24h": "the last 24 hours",
	"7d":  "the last 7 days",
}

// describe renders one field's value as a sentence fragment. str is the raw
// string for a String field; v is the number for a Number field.
func describe(f schema.Field, v float64, str string, known bool, st *Stat) string {
	if f.Kind == schema.String {
		label := docLabel(f)
		switch {
		case str == "":
			return label + " is missing"
		case !known:
			return fmt.Sprintf("%s %q was never seen in training", label, str)
		}
		return fmt.Sprintf("%s is %q", label, str)
	}

	name := f.Name
	typical := func(format func(float64) string) string {
		if st == nil || math.IsNaN(st.P50) {
			return ""
		}
		return " (typical: " + format(st.P50) + ")"
	}

	switch {
	case name == "amount":
		return "amount is " + dollars(v) + typical(dollars)
	case strings.HasPrefix(name, "distinct_cards_per_"):
		where := "this device"
		if strings.Contains(name, "_email_") {
			where = "this email domain"
		}
		if math.IsNaN(v) {
			return "no " + strings.TrimPrefix(where, "this ") + " to count cards on"
		}
		return fmt.Sprintf("%s used on %s in the last 24 hours", plural(v, "card"), where) + typical(count)
	}

	for e, noun := range entityNoun {
		rest, ok := strings.CutPrefix(name, e+"_")
		if !ok {
			continue
		}
		// Counts and sums are missing only when the payment has no key for
		// the entity at all (no card1, no device info, ...); say that,
		// rather than print a count of NaN.
		if math.IsNaN(v) && (strings.HasPrefix(rest, "txn_count_") || strings.HasPrefix(rest, "amount_sum_")) {
			return "no " + strings.TrimPrefix(noun, "this ") + " to track on this payment"
		}
		for w, span := range windowNoun {
			if rest == "txn_count_"+w {
				return fmt.Sprintf("%s on %s in %s", plural(v, "earlier payment"), noun, span) + typical(count)
			}
			if rest == "amount_sum_"+w {
				return fmt.Sprintf("%s paid on %s in %s", dollars(v), noun, span) + typical(dollars)
			}
		}
		switch rest {
		case "mean_amount_7d":
			if math.IsNaN(v) {
				return "no earlier payments on " + noun + " in the last 7 days"
			}
			return fmt.Sprintf("%s averaged %s per payment over the last 7 days", noun, dollars(v)) + typical(dollars)
		case "amount_ratio_7d":
			if math.IsNaN(v) {
				return "no earlier payments on " + noun + " in the last 7 days to compare the amount with"
			}
			return fmt.Sprintf("amount is %sx the 7-day average for %s", strconv.FormatFloat(v, 'f', 1, 64), noun) + typical(ratio)
		case "seconds_since_first":
			if math.IsNaN(v) {
				return "first payment seen from " + noun
			}
			return fmt.Sprintf("%s was first seen %s ago", noun, duration(v)) + typical(duration)
		case "seconds_since_last":
			if math.IsNaN(v) {
				return "first payment seen from " + noun
			}
			return fmt.Sprintf("previous payment on %s was %s ago", noun, duration(v)) + typical(duration)
		}
	}

	label := docLabel(f)
	if math.IsNaN(v) {
		return label + " is missing"
	}
	return label + " is " + strconv.FormatFloat(v, 'g', 6, 64) + typical(func(x float64) string { return strconv.FormatFloat(x, 'g', 6, 64) })
}

// docLabel is the field's doc up to its first parenthesis or colon:
// "purchaser email domain (P_emaildomain)" -> "purchaser email domain".
func docLabel(f schema.Field) string {
	d := f.Doc
	if i := strings.IndexAny(d, "(:"); i > 0 {
		d = d[:i]
	}
	if d = strings.TrimSpace(d); d == "" {
		return f.Name
	}
	return d
}

func count(v float64) string { return strconv.FormatFloat(math.Round(v), 'f', 0, 64) }

func ratio(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "x" }

func plural(v float64, noun string) string {
	n := math.Round(v)
	switch n {
	case 0:
		return "no " + noun + "s"
	case 1:
		return "1 " + noun
	}
	return count(n) + " " + noun + "s"
}

func dollars(v float64) string {
	s := strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	whole, cents, _ := strings.Cut(s, ".")
	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
	}
	b.WriteByte('$')
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	b.WriteByte('.')
	b.WriteString(cents)
	return b.String()
}

func duration(sec float64) string {
	unit := func(n float64, u string) string {
		n = math.Round(n)
		if n == 1 {
			return "1 " + u
		}
		return count(n) + " " + u + "s"
	}
	switch {
	case sec < 90:
		return unit(sec, "second")
	case sec < 90*60:
		return unit(sec/60, "minute")
	case sec < 48*3600:
		return unit(sec/3600, "hour")
	}
	return unit(sec/86400, "day")
}
