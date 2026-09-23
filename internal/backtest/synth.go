package backtest

import (
	"math"
	"math/rand/v2"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// SynthOptions shapes a SYNTHETIC table for tests, the differential test and
// benchmarks. Nothing produced from it is a result about real payments.
type SynthOptions struct {
	Rows int
	Seed uint64
	// Missing is an extra probability of blanking each field, on top of the
	// 1 in 5 rules.GenerateRow already leaves missing.
	Missing float64
	// Edge replaces a field with an adversarial value with this
	// probability: -0, subnormals, ±Inf, ±MaxFloat64, 0.1+0.2, text that
	// strings.ToLower changes in length or maps outside ASCII, invalid
	// UTF-8. Real features never hold some of these (Inf, say); the
	// evaluators must agree on them anyway.
	Edge float64
	// Days is the time span of DT, starting at DT = 86400 as IEEE-CIS does.
	// Zero means 182, the span of the real data.
	Days int
}

// EdgeNumbers and EdgeStrings are the adversarial values Synthetic injects.
var (
	EdgeNumbers = []float64{
		math.Copysign(0, -1), 0, 5e-324, -5e-324, 1e-300, 0.1 + 0.2, 0.3, 49.99, 1e15 + 0.3,
		math.MaxFloat64, -math.MaxFloat64, 1e308, math.Inf(1), math.Inf(-1), 9007199254740993,
	}
	EdgeStrings = []string{
		"\u212a",    // KELVIN SIGN: lowers to ASCII "k", shrinking from 3 bytes to 1
		"İstanbul",  // dotted capital I: lowers to "i̇", growing by a byte
		"ẞ",         // capital sharp s
		"ǅ",         // title-case digraph
		"ΣΑΣ",       // Greek: strings.ToLower is not context-sensitive
		"\xff\xfe",  // invalid UTF-8: each bad byte lowers to U+FFFD
		"GMAIL\xff", // invalid byte after text lowering changes
		"gmail.com", "GMAIL.COM", "Gmail.com", "k", "K", "i\u0307stanbul", " ", "a\x00b", "É", "é",
	}
)

// Synthetic builds a table of generated rows. Rows come from
// rules.GenerateRow, so rule constants from rules.Generate actually hit
// them. DT is spread evenly over the span, amounts follow the amount field
// where it is present, and fraud is drawn with a probability that rises
// with risk_score (about 3.5% overall), so sweeps and reports have a
// plausible shape. Every number derived from it is synthetic.
func Synthetic(cat *schema.Catalog, opt SynthOptions) *Table {
	rng := rand.New(rand.NewPCG(opt.Seed, 0x5eed))
	days := opt.Days
	if days <= 0 {
		days = 182
	}
	span := float64(days) * 86400
	amountSlot, hasAmount := slotOf(cat, "amount", schema.Number)
	scoreSlot, hasScore := slotOf(cat, "risk_score", schema.Number)
	b := NewBuilder(cat, opt.Rows)
	for i := range opt.Rows {
		row := rules.GenerateRow(rng, cat)
		for _, f := range cat.Fields() {
			switch {
			case opt.Missing > 0 && rng.Float64() < opt.Missing:
				if f.Kind == schema.Number {
					row.Num[f.Slot] = math.NaN()
				} else {
					row.Str[f.Slot] = ""
				}
			case opt.Edge > 0 && rng.Float64() < opt.Edge:
				if f.Kind == schema.Number {
					row.Num[f.Slot] = EdgeNumbers[rng.IntN(len(EdgeNumbers))]
				} else {
					row.Str[f.Slot] = EdgeStrings[rng.IntN(len(EdgeStrings))]
				}
			}
		}
		amount := math.Round(math.Exp(3.5+rng.NormFloat64())*100) / 100
		if hasAmount {
			if v := row.Num[amountSlot]; v > 0 && v < 1e6 {
				amount = v
			}
		}
		p := 0.005
		if hasScore {
			if s := row.Num[scoreSlot]; s >= 0 && s <= 99 {
				p += 0.12 * math.Pow(s/99, 3)
			}
		}
		label := Legit
		if rng.Float64() < p {
			label = Fraud
		}
		dt := 86400 + int64(span*float64(i)/float64(max(opt.Rows, 1)))
		b.Append(row, Meta{ID: int64(3_000_000 + i), DT: dt, Amount: amount, Fraud: label})
	}
	return b.Table()
}

func slotOf(cat *schema.Catalog, name string, kind schema.Kind) (int, bool) {
	f, ok := cat.Lookup(name)
	if !ok || f.Kind != kind {
		return 0, false
	}
	return f.Slot, true
}
