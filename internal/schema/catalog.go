// Package schema defines the field catalog: every attribute a rule can name and
// the model can read, with its type. It is the one contract shared by the
// feature engine, the rule compiler, the model encoder, and the backtester.
package schema

import (
	"fmt"
	"math"
	"sort"
)

// Kind is a field's type. Missing is a value, not a kind: NaN for numbers and
// the empty string for strings.
type Kind uint8

const (
	Number Kind = iota + 1
	String
)

func (k Kind) String() string {
	switch k {
	case Number:
		return "number"
	case String:
		return "string"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// Field is one attribute. Slot indexes Row.Num for numbers and Row.Str for strings.
type Field struct {
	Name string
	Kind Kind
	Slot int
	Doc  string
}

// Catalog is an ordered, immutable set of fields.
type Catalog struct {
	fields []Field
	byName map[string]int
	nNum   int
	nStr   int
}

// Row is one transaction's feature vector. A missing number is NaN and a
// missing string is "". No field ever holds a meaningful empty string.
type Row struct {
	Num []float64
	Str []string
}

// Missing is the value used for a missing number.
var Missing = math.NaN()

// Entities and windows of the velocity features, in catalog order.
var (
	Entities = []string{"card", "uid", "device", "email"}
	Windows  = []struct {
		Name    string
		Seconds int64
	}{{"1h", 3600}, {"24h", 86400}, {"7d", 7 * 86400}}
)

// RawFields are taken from the transaction itself.
var RawFields = []Field{
	{Name: "amount", Kind: Number, Doc: "payment amount in dollars (TransactionAmt)"},
	{Name: "product_code", Kind: String, Doc: "product code (ProductCD): W, C, R, H, S"},
	{Name: "card_network", Kind: String, Doc: "card network (card4): visa, mastercard, ..."},
	{Name: "card_type", Kind: String, Doc: "card type (card6): debit, credit, ..."},
	{Name: "purchaser_email_domain", Kind: String, Doc: "purchaser email domain (P_emaildomain)"},
	{Name: "recipient_email_domain", Kind: String, Doc: "recipient email domain (R_emaildomain)"},
	{Name: "device_type", Kind: String, Doc: "device type (DeviceType): desktop, mobile"},
	{Name: "device_info", Kind: String, Doc: "device info string (DeviceInfo)"},
	{Name: "distance", Kind: Number, Doc: "anonymized distance (dist1)"},
	{Name: "billing_region", Kind: Number, Doc: "anonymized billing region code (addr1)"},
	{Name: "billing_country_code", Kind: Number, Doc: "anonymized billing country code (addr2)"},
}

// VelocityFieldNames lists RiskGate's streaming features in catalog order.
// Counts and sums cover strictly earlier transactions of the same entity
// inside the trailing window; the current transaction is never included.
func VelocityFieldNames() []Field {
	var out []Field
	for _, e := range Entities {
		for _, w := range Windows {
			out = append(out,
				Field{Name: e + "_txn_count_" + w.Name, Kind: Number, Doc: fmt.Sprintf("earlier payments by this %s in the last %s", e, w.Name)},
				Field{Name: e + "_amount_sum_" + w.Name, Kind: Number, Doc: fmt.Sprintf("dollars paid by this %s in the last %s", e, w.Name)},
			)
		}
		out = append(out,
			Field{Name: e + "_mean_amount_7d", Kind: Number, Doc: fmt.Sprintf("mean earlier payment by this %s over 7d (missing if none)", e)},
			Field{Name: e + "_amount_ratio_7d", Kind: Number, Doc: fmt.Sprintf("amount divided by %s_mean_amount_7d", e)},
			Field{Name: e + "_seconds_since_first", Kind: Number, Doc: fmt.Sprintf("seconds since this %s was first seen (missing if new)", e)},
			Field{Name: e + "_seconds_since_last", Kind: Number, Doc: fmt.Sprintf("seconds since this %s's previous payment (missing if new)", e)},
		)
	}
	out = append(out,
		Field{Name: "distinct_cards_per_device_24h", Kind: Number, Doc: "distinct cards seen on this device in the last 24h, excluding this payment"},
		Field{Name: "distinct_cards_per_email_24h", Kind: Number, Doc: "distinct cards seen with this purchaser email domain in the last 24h, excluding this payment"},
	)
	return out
}

// RiskScoreField is filled by the model after features are computed.
var RiskScoreField = Field{Name: "risk_score", Kind: Number, Doc: "calibrated fraud probability scaled to 0-99 (not a percentile)"}

// Default returns the full catalog: raw fields, velocity features, risk_score.
func Default() *Catalog {
	fs := append(append(append([]Field{}, RawFields...), VelocityFieldNames()...), RiskScoreField)
	return New(fs)
}

// New builds a catalog, assigning slots in order. It panics on duplicate names.
func New(fields []Field) *Catalog {
	c := &Catalog{byName: make(map[string]int, len(fields))}
	for _, f := range fields {
		if _, dup := c.byName[f.Name]; dup {
			panic("schema: duplicate field " + f.Name)
		}
		switch f.Kind {
		case Number:
			f.Slot = c.nNum
			c.nNum++
		case String:
			f.Slot = c.nStr
			c.nStr++
		default:
			panic("schema: bad kind for " + f.Name)
		}
		c.byName[f.Name] = len(c.fields)
		c.fields = append(c.fields, f)
	}
	return c
}

func (c *Catalog) Lookup(name string) (Field, bool) {
	i, ok := c.byName[name]
	if !ok {
		return Field{}, false
	}
	return c.fields[i], true
}

// MustLookup panics if name is not in the catalog. For code, not user input.
func (c *Catalog) MustLookup(name string) Field {
	f, ok := c.Lookup(name)
	if !ok {
		panic("schema: unknown field " + name)
	}
	return f
}

// Fields returns the fields in catalog order. Do not modify the result.
func (c *Catalog) Fields() []Field { return c.fields }

// Names returns all field names, sorted, for suggestions and docs.
func (c *Catalog) Names() []string {
	out := make([]string, len(c.fields))
	for i, f := range c.fields {
		out[i] = f.Name
	}
	sort.Strings(out)
	return out
}

func (c *Catalog) NumCount() int { return c.nNum }
func (c *Catalog) StrCount() int { return c.nStr }

// NewRow returns a row with every field missing.
func (c *Catalog) NewRow() Row {
	r := Row{Num: make([]float64, c.nNum), Str: make([]string, c.nStr)}
	for i := range r.Num {
		r.Num[i] = math.NaN()
	}
	return r
}

// Clone deep-copies a row.
func (r Row) Clone() Row {
	return Row{Num: append([]float64(nil), r.Num...), Str: append([]string(nil), r.Str...)}
}
