// Package synth generates SYNTHETIC transactions in the IEEE-CIS shape, so
// the whole pipeline can run before the real data is downloaded.
//
// Nothing produced from this data is a result. Every tool that reads it says
// SYNTHETIC, and the generator writes a marker file (data.SyntheticMarker)
// next to its CSVs so the loader knows.
//
// The marginals are loosely modelled on published summaries of IEEE-CIS
// (product mix, card networks, email domains, how often identity, dist1,
// and addr1 are missing). Fraud comes from three planted patterns:
//
//   - Card testing: bursts of small payments from one device across many
//     stolen cards within minutes. Device velocity and distinct cards per
//     device find it.
//   - Account takeover: a customer with a history suddenly spends several
//     times their usual amount, several times in a few hours, from a new
//     device. The uid amount ratio and uid velocity find it.
//   - Raw-field fraud: single payments whose only tell is a shift in raw
//     fields (product C, credit cards, risky email domains, missing billing
//     address, long distance). Velocity features add little here, so the
//     raw-only baseline has something real to find too.
//
// Legitimate traffic includes look-alikes (shared devices such as "Windows"
// with many cards, customers' occasional big purchases, short legitimate
// bursts), so no single feature separates the classes.
package synth

import (
	"math"
	"math/rand/v2"
	"slices"
	"sort"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

// Config controls the generator. Zero fields take the defaults, which match
// the size of the IEEE-CIS training file.
type Config struct {
	Rows      int     // total transactions (default 590,540)
	Days      int     // span in days (default 182)
	FraudRate float64 // target share of fraud (default 0.035)
	Customers int     // legitimate customers (default 120,000)
	Seed      uint64  // generator seed (default 1)
}

func (c *Config) setDefaults() {
	if c.Rows == 0 {
		c.Rows = 590540
	}
	if c.Days == 0 {
		c.Days = 182
	}
	if c.FraudRate == 0 {
		c.FraudRate = 0.035
	}
	if c.Customers == 0 {
		c.Customers = max(100, c.Rows/5)
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
}

// Pattern names, as counted in Summary.ByPattern.
const (
	PatternCardTesting     = "card_testing"
	PatternAccountTakeover = "account_takeover"
	PatternRawFields       = "raw_fields"
)

// Summary describes a generated set.
type Summary struct {
	Rows, Fraud  int
	ByPattern    map[string]int
	WithIdentity int
	FirstDT      int64
	LastDT       int64
}

// FirstID is the first TransactionID.
const FirstID = 3000000

// startDT is the first second of the data, as in IEEE-CIS.
const startDT = 86400

type customer struct {
	card1, addr1, addr2 float64
	card4, card6        string
	firstUseDay         int64 // D1 is the transaction day minus this
	pEmail, rEmail      string
	deviceType          string
	deviceInfo          string
	identity            bool
	product             string
	scale               float64 // multiplies the product's typical amount
	homeDist            float64 // NaN when dist1 is not recorded
}

type gen struct {
	cfg       Config
	r         *rand.Rand
	cards     []cardInfo
	cardPick  *picker
	customers []customer
	custPick  *picker
	out       []data.Txn
	outCust   []int32 // customer of each legitimate transaction in out
	summary   Summary
}

type cardInfo struct {
	card1        float64
	card4, card6 string
}

// Generate builds cfg.Rows transactions, sorted by data.Less, with
// TransactionIDs assigned in time order from FirstID.
func Generate(cfg Config) ([]data.Txn, Summary) {
	cfg.setDefaults()
	g := &gen{
		cfg:     cfg,
		r:       rand.New(rand.NewPCG(cfg.Seed, 0x5eed_2017_1201)),
		summary: Summary{ByPattern: map[string]int{}},
	}
	g.makeCards()
	g.makeCustomers()

	// Legitimate traffic first: account takeovers are anchored to it.
	nFraud := int(math.Round(float64(cfg.Rows) * cfg.FraudRate))
	g.legitimate(cfg.Rows - nFraud)
	g.cardTesting(nFraud * 40 / 100)
	g.accountTakeover(nFraud * 30 / 100)
	g.rawFieldFraud(nFraud - g.summary.Fraud)

	// Sort by time; generation order breaks ties so the result is
	// deterministic, and IDs then follow time as they do in IEEE-CIS.
	idx := make([]int, len(g.out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return g.out[idx[a]].DT < g.out[idx[b]].DT })
	txns := make([]data.Txn, len(g.out))
	for i, j := range idx {
		txns[i] = g.out[j]
		txns[i].ID = FirstID + int64(i)
		if txns[i].DeviceType != "" || txns[i].DeviceInfo != "" {
			g.summary.WithIdentity++
		}
	}
	g.summary.Rows = len(txns)
	if len(txns) > 0 {
		g.summary.FirstDT, g.summary.LastDT = txns[0].DT, txns[len(txns)-1].DT
	}
	return txns, g.summary
}

func (g *gen) endDT() int64 { return startDT + int64(g.cfg.Days)*data.SecondsPerDay }

// weighted is a categorical distribution over values.
type weighted[T any] struct {
	vals []T
	pick *picker
}

func newWeighted[T any](vals []T, weights []float64) weighted[T] {
	return weighted[T]{vals: vals, pick: newPicker(weights)}
}

func (w weighted[T]) draw(r *rand.Rand) T { return w.vals[w.pick.draw(r)] }

// picker draws an index with probability proportional to its weight, by
// binary search over the cumulative weights.
type picker struct{ cum []float64 }

func newPicker(weights []float64) *picker {
	p := &picker{cum: make([]float64, len(weights))}
	total := 0.0
	for i, w := range weights {
		total += w
		p.cum[i] = total
	}
	return p
}

func (p *picker) draw(r *rand.Rand) int {
	x := r.Float64() * p.cum[len(p.cum)-1]
	return min(sort.SearchFloat64s(p.cum, x), len(p.cum)-1)
}

var (
	emailDomains = newWeighted(
		[]string{"gmail.com", "yahoo.com", "hotmail.com", "anonymous.com", "aol.com", "comcast.net",
			"icloud.com", "outlook.com", "msn.com", "att.net", "live.com", "sbcglobal.net",
			"verizon.net", "ymail.com", "bellsouth.net", "yahoo.com.mx", "me.com", "cox.net",
			"optonline.net", "charter.net", "live.com.mx", "rocketmail.com", "mail.com",
			"earthlink.net", "outlook.es", "hotmail.es", "protonmail.com", ""},
		[]float64{38.7, 17.1, 7.7, 6.3, 4.8, 1.3, 1.1, 0.9, 0.7, 0.7, 0.5, 0.5,
			0.5, 0.2, 0.3, 0.3, 0.2, 0.2, 0.2, 0.1, 0.1, 0.1, 0.1,
			0.1, 0.04, 0.03, 0.01, 16})
	riskyEmail = newWeighted(
		[]string{"anonymous.com", "gmail.com", "hotmail.com", "outlook.com", "protonmail.com", "mail.com", "outlook.es", ""},
		[]float64{30, 25, 12, 8, 6, 6, 3, 10})
	desktopDevices = newWeighted(
		[]string{"Windows", "MacOS", "Trident/7.0", "rv:11.0", "Linux", "rv:57.0", "rv:59.0", "rv:52.0"},
		[]float64{50, 22, 12, 5, 3, 3, 3, 2})
	// Device strings are invented, shaped like the identity table's (an OS
	// name for most desktops, a model and build for Android phones).
	mobileDevices = newWeighted(
		[]string{"iOS", "Phone A1 Build/AB10", "Phone A2 Build/AB12", "Phone B7 Build/CD31",
			"Phone C3 Plus Build/EF22", "Phone D5 Build/GH08", "Phone E9 Build/JK19",
			"Phone F2 Build/LM44", "Phone G6 Build/NP07", "Phone H4 Build/QR63"},
		[]float64{45, 8, 6, 5, 4, 3, 3, 3, 2, 2})
	products         = newWeighted([]string{"W", "C", "H", "R", "S"}, []float64{80, 10, 5, 3, 2})
	rawFraudProducts = newWeighted([]string{"W", "C", "H", "R", "S"}, []float64{40, 30, 10, 10, 10})
	networks         = newWeighted([]string{"visa", "mastercard", "american express", "discover", ""},
		[]float64{65, 32, 1.4, 1.1, 0.3})
	cardTypes = newWeighted([]string{"debit", "credit"}, []float64{75, 25})
)

// makeCards draws the card1 values. Popularity follows a power law, as card1
// behaves like an issuer-level identifier shared by many customers.
func (g *gen) makeCards() {
	pool := make([]float64, 0, 17397)
	for v := 1000; v <= 18396; v++ {
		pool = append(pool, float64(v))
	}
	g.r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	n := min(12000, len(pool))
	g.cards = make([]cardInfo, n)
	weights := make([]float64, n)
	for i := range g.cards {
		g.cards[i] = cardInfo{card1: pool[i], card4: networks.draw(g.r), card6: cardTypes.draw(g.r)}
		weights[i] = 1 / math.Pow(float64(i+10), 0.9)
	}
	g.cardPick = newPicker(weights)
}

// uniqueDevice invents a rarely-seen phone build string.
func (g *gen) uniqueDevice() string {
	const letters = "ABCDEFGHJKLMNPRSTUVWXZ"
	b := []byte("Phone-")
	b = append(b, letters[g.r.IntN(len(letters))])
	for range 3 {
		b = append(b, byte('0'+g.r.IntN(10)))
	}
	b = append(b, letters[g.r.IntN(len(letters))])
	b = append(b, " Build/"...)
	for range 3 {
		b = append(b, letters[g.r.IntN(len(letters))])
	}
	for range 2 {
		b = append(b, byte('0'+g.r.IntN(10)))
	}
	return string(b)
}

// device draws an identity-table device, with DeviceInfo sometimes missing.
func (g *gen) device() (deviceType, info string) {
	if g.r.Float64() < 0.55 {
		deviceType, info = "desktop", desktopDevices.draw(g.r)
	} else {
		deviceType = "mobile"
		if g.r.Float64() < 0.25 {
			info = g.uniqueDevice()
		} else {
			info = mobileDevices.draw(g.r)
		}
	}
	if g.r.Float64() < 0.15 {
		info = ""
	}
	return deviceType, info
}

func (g *gen) region() (addr1, addr2 float64) {
	if g.r.Float64() < 0.11 {
		return math.NaN(), math.NaN()
	}
	// Region codes 100-540 with a power law, as addr1 is concentrated.
	addr1 = float64(100 + int(440*math.Pow(g.r.Float64(), 2.2)))
	addr2 = 87
	if g.r.Float64() < 0.03 {
		addr2 = []float64{60, 96, 32, 65, 16, 31}[g.r.IntN(6)]
	}
	return addr1, addr2
}

func (g *gen) makeCustomers() {
	day0 := int64(startDT / data.SecondsPerDay)
	g.customers = make([]customer, g.cfg.Customers)
	weights := make([]float64, len(g.customers))
	for i := range g.customers {
		c := &g.customers[i]
		card := g.cards[g.cardPick.draw(g.r)]
		c.card1, c.card4, c.card6 = card.card1, card.card4, card.card6
		c.addr1, c.addr2 = g.region()
		c.product = products.draw(g.r)
		c.pEmail = emailDomains.draw(g.r)
		if g.r.Float64() < 0.2 {
			c.rEmail = c.pEmail
			if g.r.Float64() < 0.3 {
				c.rEmail = emailDomains.draw(g.r)
			}
		}
		// Identity rows are rare for W and common for the other products.
		pIdentity := 0.12
		if c.product != "W" {
			pIdentity = 0.85
		}
		if g.r.Float64() < pIdentity {
			c.identity = true
			c.deviceType, c.deviceInfo = g.device()
		}
		c.scale = math.Exp(g.r.NormFloat64() * 0.5)
		c.homeDist = math.NaN()
		if c.product == "W" && g.r.Float64() < 0.55 {
			c.homeDist = g.r.ExpFloat64() * 30
		}
		// Most cards were first used before the data starts; some start
		// during it.
		if g.r.Float64() < 0.85 {
			c.firstUseDay = day0 - int64(min(640, g.r.ExpFloat64()*200))
		} else {
			c.firstUseDay = day0 + g.r.Int64N(int64(g.cfg.Days))
		}
		active := float64(day0+int64(g.cfg.Days)-max(c.firstUseDay, day0)) / float64(g.cfg.Days)
		weights[i] = math.Exp(g.r.NormFloat64()*1.2) * active
	}
	g.custPick = newPicker(weights)
}

// timeOfDay draws seconds into a day with more traffic in the afternoon and
// evening than at night.
func (g *gen) timeOfDay() int64 {
	for {
		s := g.r.Int64N(data.SecondsPerDay)
		hour := float64(s) / 3600
		if g.r.Float64() < 0.35+0.65*(0.5-0.5*math.Cos((hour-4)/24*2*math.Pi)) {
			return s
		}
	}
}

// timeFrom draws a timestamp at or after the given day, within the data.
func (g *gen) timeFrom(fromDay int64) int64 {
	lo := max(fromDay*data.SecondsPerDay, startDT)
	days := (g.endDT() - lo) / data.SecondsPerDay
	if days <= 0 {
		return g.endDT() - 1 - g.r.Int64N(data.SecondsPerDay)
	}
	return lo + g.r.Int64N(days)*data.SecondsPerDay + g.timeOfDay()
}

func round(x float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(x*p) / p
}

// amount draws a payment for a product, scaled for the customer.
func (g *gen) amount(product string, scale float64) float64 {
	switch product {
	case "C":
		return max(0.25, round(math.Exp(math.Log(30)+g.r.NormFloat64()*0.9)*scale, 3))
	case "H":
		return []float64{25, 35, 50, 50, 75, 100, 100, 150, 200}[g.r.IntN(9)]
	case "R":
		return []float64{50, 100, 100, 150, 200, 250, 300, 400, 500}[g.r.IntN(9)]
	case "S":
		return max(1, round(math.Exp(math.Log(25)+g.r.NormFloat64()*0.7)*scale, 2))
	}
	a := max(1, math.Exp(math.Log(60)+g.r.NormFloat64()*0.8)*scale)
	if g.r.Float64() < 0.2 {
		return math.Floor(a) + 0.95
	}
	return round(a, 2)
}

// txnFor builds a transaction for customer c at time dt.
func (g *gen) txnFor(c *customer, dt int64, amount float64) data.Txn {
	t := data.Txn{
		DT: dt, Amount: amount, ProductCode: c.product,
		Card1: c.card1, Card4: c.card4, Card6: c.card6,
		Addr1: c.addr1, Addr2: c.addr2, Dist1: math.NaN(),
		PEmail: c.pEmail, REmail: c.rEmail,
		D1: float64(min(640, dt/data.SecondsPerDay-c.firstUseDay)),
	}
	if !math.IsNaN(c.homeDist) {
		t.Dist1 = math.Round(c.homeDist + g.r.ExpFloat64()*5)
	}
	if g.r.Float64() < 0.002 {
		t.D1 = math.NaN()
	}
	if c.identity {
		t.DeviceType, t.DeviceInfo = c.deviceType, c.deviceInfo
	}
	return t
}

func (g *gen) emit(t data.Txn, pattern string) {
	if pattern != "" {
		t.IsFraud = 1
		g.summary.Fraud++
		g.summary.ByPattern[pattern]++
	}
	g.out = append(g.out, t)
}

func (g *gen) legitimate(n int) {
	for n > 0 {
		ci := g.custPick.draw(g.r)
		c := &g.customers[ci]
		dt := g.timeFrom(c.firstUseDay)
		amt := g.amount(c.product, c.scale)
		if g.r.Float64() < 0.02 { // an occasional big purchase
			amt = round(amt*(3+5*g.r.Float64()), 2)
		}
		g.emit(g.txnFor(c, dt, amt), "")
		g.outCust = append(g.outCust, int32(ci))
		n--
		// A short legitimate session: a few more payments within minutes.
		for n > 0 && g.r.Float64() < 0.15 {
			dt += 60 + g.r.Int64N(1800)
			if dt >= g.endDT() {
				break
			}
			g.emit(g.txnFor(c, dt, g.amount(c.product, c.scale)), "")
			g.outCust = append(g.outCust, int32(ci))
			n--
		}
	}
}

// cardTesting plants bursts: one device, many stolen cards, small amounts,
// minutes apart. Most attacker devices are rare strings; some hide behind
// common ones like "Windows".
func (g *gen) cardTesting(n int) {
	for n > 0 {
		size := min(n, 8+g.r.IntN(33))
		start := g.timeFrom(0)
		duration := int64(600 + g.r.IntN(4800))
		deviceType, info := "mobile", g.uniqueDevice()
		if g.r.Float64() < 0.3 {
			deviceType, info = g.device()
		}
		email := riskyEmail.draw(g.r)
		times := make([]int64, size)
		for i := range times {
			times[i] = min(start+g.r.Int64N(duration), g.endDT()-1)
		}
		slices.Sort(times)
		for _, dt := range times {
			victim := g.customers[g.r.IntN(len(g.customers))]
			if g.r.Float64() < 0.3 { // the attacker lacks the billing address
				victim.addr1, victim.addr2 = math.NaN(), math.NaN()
			}
			victim.product = "W"
			if g.r.Float64() < 0.4 {
				victim.product = "C"
			}
			victim.identity, victim.deviceType, victim.deviceInfo = true, deviceType, info
			victim.pEmail, victim.rEmail = email, ""
			victim.homeDist = math.NaN()
			if victim.firstUseDay > dt/data.SecondsPerDay {
				victim.firstUseDay = dt / data.SecondsPerDay
			}
			amt := round(0.5+g.r.Float64()*14.5, 2)
			if victim.product == "C" {
				amt = round(amt, 3)
			}
			g.emit(g.txnFor(&victim, dt, amt), PatternCardTesting)
		}
		n -= size
	}
}

// accountTakeover plants a few large payments on an established customer's
// uid, hours apart, from a new device. Each takeover starts one hour to
// three days after one of the victim's own payments, so there is recent
// history for the takeover to look unlike.
func (g *gen) accountTakeover(n int) {
	for n > 0 {
		j := g.r.IntN(len(g.outCust)) // drawing a payment favours active customers
		c := g.customers[g.outCust[j]]
		start := g.out[j].DT + 3600 + g.r.Int64N(71*3600)
		if math.IsNaN(c.addr1) || start >= g.endDT() { // the uid needs a billing region
			continue
		}
		size := min(n, 2+g.r.IntN(7))
		span := int64(1800 + g.r.IntN(8*3600))
		c.identity = g.r.Float64() < 0.7
		if c.identity {
			c.deviceType, c.deviceInfo = g.device()
			if g.r.Float64() < 0.5 {
				c.deviceInfo = g.uniqueDevice()
			}
		}
		if g.r.Float64() < 0.5 {
			c.pEmail = riskyEmail.draw(g.r)
		}
		if c.product == "C" || c.product == "S" {
			c.product = "W"
		}
		for range size {
			dt := min(start+g.r.Int64N(span), g.endDT()-1)
			amt := round(g.amount(c.product, c.scale)*(4+16*g.r.Float64()), 2)
			t := g.txnFor(&c, dt, amt)
			if !math.IsNaN(t.Dist1) {
				t.Dist1 = math.Round(300 + g.r.Float64()*2700)
			}
			g.emit(t, PatternAccountTakeover)
		}
		n -= size
	}
}

// rawFieldFraud plants single payments whose raw fields lean risky.
func (g *gen) rawFieldFraud(n int) {
	day0 := int64(startDT / data.SecondsPerDay)
	for range n {
		card := g.cards[g.cardPick.draw(g.r)]
		c := customer{card1: card.card1, card4: card.card4, card6: card.card6, homeDist: math.NaN(), scale: 1}
		if g.r.Float64() < 0.65 {
			c.card6 = "credit"
		}
		c.addr1, c.addr2 = g.region()
		if g.r.Float64() < 0.35 {
			c.addr1, c.addr2 = math.NaN(), math.NaN()
		} else if g.r.Float64() < 0.2 {
			c.addr2 = []float64{60, 96, 32, 65}[g.r.IntN(4)]
		}
		c.product = rawFraudProducts.draw(g.r)
		if g.r.Float64() < 0.6 {
			c.pEmail = riskyEmail.draw(g.r)
			if g.r.Float64() < 0.4 {
				c.rEmail = c.pEmail
			}
		} else {
			c.pEmail = emailDomains.draw(g.r)
		}
		if c.product != "W" || g.r.Float64() < 0.4 {
			c.identity = true
			c.deviceType, c.deviceInfo = g.device()
		}
		dt := g.timeFrom(day0)
		c.firstUseDay = dt/data.SecondsPerDay - int64(g.r.IntN(3))
		t := g.txnFor(&c, dt, g.amount(c.product, math.Exp(g.r.NormFloat64()*0.6)))
		if g.r.Float64() < 0.4 {
			t.Dist1 = math.Round(100 + g.r.ExpFloat64()*800)
		}
		g.emit(t, PatternRawFields)
	}
}
