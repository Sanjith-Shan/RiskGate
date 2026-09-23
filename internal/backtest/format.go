package backtest

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Plain-English formatting for summaries a merchant reads. The rules are
// small but easy to get wrong in ways a reader notices at once: "1
// payments", "$1234567.8", "0% of fraud" when the true figure is 0.3%, or
// "100%" when one payment in a thousand was legitimate.

// DefaultEpoch is the wall-clock time assigned to TransactionDT = 0. The
// IEEE-CIS data does not state it; 2017-12-01 00:00 UTC is the start date
// commonly assumed in the competition's discussion (TransactionDT begins at
// 86400, one day in) and is what RiskGate's replay uses. It is an
// assumption, and every date the backtester prints depends on it.
var DefaultEpoch = time.Date(2017, 12, 1, 0, 0, 0, 0, time.UTC)

// commas formats an integer with thousands separators: 1204 -> "1,204".
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// count formats n with a noun in the right number: "1 payment",
// "1,204 payments".
func count(n int, singular, pluralForm string) string {
	if n == 1 {
		return "1 " + singular
	}
	return commas(int64(n)) + " " + pluralForm
}

// dollars formats an amount. Under $100 it keeps cents when there are any
// ("$4.99", "$25"); from $100 up it rounds to whole dollars ("$182,347"),
// because cents on a large total are noise.
func dollars(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	if v < 100 {
		c := math.Round(v * 100)
		if math.Mod(c, 100) != 0 {
			return sign + "$" + strconv.FormatFloat(c/100, 'f', 2, 64)
		}
	}
	return sign + "$" + commas(int64(math.Round(v)))
}

// percent formats num/den as a percentage a reader can trust at the ends of
// the scale: a small nonzero share is never "0%" and a share short of all is
// never "100%".
func percent(num, den float64) string {
	if den == 0 {
		return "n/a"
	}
	p := 100 * num / den
	switch {
	case p == 0:
		return "0%"
	case p < 0.1:
		return "under 0.1%"
	case p < 1:
		return strconv.FormatFloat(p, 'f', 1, 64) + "%"
	case p > 99 && num < den:
		return "over 99%"
	}
	return strconv.FormatFloat(math.Round(p), 'f', 0, 64) + "%"
}

// days formats a duration in seconds as whole days: "30 days", "1 day".
func days(seconds int64) string {
	return count(int(math.Round(float64(seconds)/86400)), "day", "days")
}

// date formats a TransactionDT as a calendar date under the epoch.
func date(epoch time.Time, dt int64) string {
	return epoch.Add(time.Duration(dt) * time.Second).Format("January 2, 2006")
}

// capitalize upper-cases the first letter of a sentence.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
