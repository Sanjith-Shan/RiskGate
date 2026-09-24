// Package features computes RiskGate's feature vector for a payment: the raw
// fields it carries and the velocity features of its card, approximate
// customer (uid), device, and email domain over trailing windows.
//
// There is one implementation. The offline export, the backtester, and the
// online service all call Engine.ScoreAndUpdate, the export and backtester
// through Replay, the service once per request. A feature therefore cannot
// mean one thing in training and another in production.
//
// Point-in-time correctness: a payment's features come from payments
// strictly before it. Replay feeds payments in (TransactionDT,
// TransactionID) order and scores each one before adding it to the state,
// and the leakage test checks the property directly.
package features

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// DefaultEvictEvery is how many updates pass between idle-key sweeps.
const DefaultEvictEvery = 1 << 16

// Engine fills schema rows from a State. Its methods are safe for
// concurrent use exactly when its State is (see NewSharded).
type Engine struct {
	st  State
	cat *schema.Catalog

	raw      rawSlots
	vel      [NumEntities]entitySlots
	risk     int
	extraNum []int // catalog fields the engine does not produce: left missing
	extraStr []int

	evictEvery uint64
	updates    atomic.Uint64
}

type rawSlots struct {
	amount, distance, region, country                            int
	product, network, cardType, pEmail, rEmail, devType, devInfo int
}

type entitySlots struct {
	count, sum                         [NumWindows]int
	mean, ratio, sinceFirst, sinceLast int
	distinct                           int // -1 if the entity has none
}

// NewEngine binds the engine to a catalog, which must contain every raw
// field and velocity feature (schema.Default does). Catalog fields the engine
// does not know are always missing, and risk_score is left NaN for the model.
func NewEngine(cat *schema.Catalog, st State) (*Engine, error) {
	e := &Engine{st: st, cat: cat, evictEvery: DefaultEvictEvery}
	known := map[string]bool{}
	var err error
	slot := func(name string, kind schema.Kind) int {
		known[name] = true
		f, ok := cat.Lookup(name)
		switch {
		case !ok:
			err = fmt.Errorf("features: catalog has no field %q", name)
		case f.Kind != kind:
			err = fmt.Errorf("features: catalog field %q is a %v, want a %v", name, f.Kind, kind)
		}
		return f.Slot
	}
	num := func(name string) int { return slot(name, schema.Number) }
	str := func(name string) int { return slot(name, schema.String) }

	e.raw = rawSlots{
		amount: num("amount"), distance: num("distance"),
		region: num("billing_region"), country: num("billing_country_code"),
		product: str("product_code"), network: str("card_network"), cardType: str("card_type"),
		pEmail: str("purchaser_email_domain"), rEmail: str("recipient_email_domain"),
		devType: str("device_type"), devInfo: str("device_info"),
	}
	for ent := range Entity(NumEntities) {
		s := &e.vel[ent]
		p := ent.String() + "_"
		for w, win := range schema.Windows {
			s.count[w] = num(p + "txn_count_" + win.Name)
			s.sum[w] = num(p + "amount_sum_" + win.Name)
		}
		s.mean = num(p + "mean_amount_7d")
		s.ratio = num(p + "amount_ratio_7d")
		s.sinceFirst = num(p + "seconds_since_first")
		s.sinceLast = num(p + "seconds_since_last")
		s.distinct = -1
		if ent.tracksDistinct() {
			s.distinct = num("distinct_cards_per_" + ent.String() + "_24h")
		}
	}
	e.risk = num(schema.RiskScoreField.Name)
	if err != nil {
		return nil, err
	}
	for _, f := range cat.Fields() {
		if known[f.Name] {
			continue
		}
		if f.Kind == schema.Number {
			e.extraNum = append(e.extraNum, f.Slot)
		} else {
			e.extraStr = append(e.extraStr, f.Slot)
		}
	}
	return e, nil
}

// Catalog returns the engine's catalog.
func (e *Engine) Catalog() *schema.Catalog { return e.cat }

// State returns the engine's state.
func (e *Engine) State() State { return e.st }

// SetEvictEvery sets how many updates pass between idle-key sweeps; 0 turns
// the automatic sweep off (call State().EvictIdle yourself). Sweeps only
// free memory: idle keys already read as unseen, so results do not depend on
// when, or whether, a sweep runs. Call before first use.
func (e *Engine) SetEvictEvery(n uint64) { e.evictEvery = n }

// NewRow returns a row sized for the engine's catalog.
func (e *Engine) NewRow() schema.Row { return e.cat.NewRow() }

// Score returns t's features against the current state without changing
// it. It allocates the row; ScoreInto does not.
func (e *Engine) Score(t *data.Txn) schema.Row {
	row := e.cat.NewRow()
	e.ScoreInto(t, row)
	return row
}

// ScoreInto writes t's features into row, which must come from NewRow. Every
// slot is written.
func (e *Engine) ScoreInto(t *data.Txn, row schema.Row) {
	e.fillRaw(t, row)
	keys, ok := KeysOf(t)
	for ent := range Entity(NumEntities) {
		if !ok[ent] {
			e.fillMissing(ent, row)
			continue
		}
		a := e.st.Read(keys[ent], t.DT)
		e.fillEntity(ent, &a, t, row)
	}
}

// Update adds t to the state.
func (e *Engine) Update(t *data.Txn) {
	ev := EventOf(t)
	keys, ok := KeysOf(t)
	for ent := range Entity(NumEntities) {
		if ok[ent] {
			e.st.Add(keys[ent], ev)
		}
	}
	e.maybeEvict(t.DT)
}

// ScoreAndUpdate is ScoreInto then Update, with each key read and updated
// under one lock (State.ReadAdd), so that concurrent payments on the same
// key each see the other either entirely or not at all. Keys are
// independent, so per-key atomicity is all a payment needs. This is the one
// entry point Replay and the online service share.
func (e *Engine) ScoreAndUpdate(t *data.Txn, row schema.Row) {
	e.fillRaw(t, row)
	ev := EventOf(t)
	keys, ok := KeysOf(t)
	for ent := range Entity(NumEntities) {
		if !ok[ent] {
			e.fillMissing(ent, row)
			continue
		}
		a := e.st.ReadAdd(keys[ent], ev)
		e.fillEntity(ent, &a, t, row)
	}
	e.maybeEvict(t.DT)
}

func (e *Engine) maybeEvict(now int64) {
	if e.evictEvery > 0 && e.updates.Add(1)%e.evictEvery == 0 {
		e.st.EvictIdle(now)
	}
}

func (e *Engine) fillRaw(t *data.Txn, row schema.Row) {
	r := &e.raw
	row.Num[r.amount] = t.Amount
	row.Num[r.distance] = t.Dist1
	row.Num[r.region] = t.Addr1
	row.Num[r.country] = t.Addr2
	row.Str[r.product] = t.ProductCode
	row.Str[r.network] = t.Card4
	row.Str[r.cardType] = t.Card6
	row.Str[r.pEmail] = t.PEmail
	row.Str[r.rEmail] = t.REmail
	row.Str[r.devType] = t.DeviceType
	row.Str[r.devInfo] = t.DeviceInfo
	row.Num[e.risk] = math.NaN() // filled by the model, never by features
	for _, i := range e.extraNum {
		row.Num[i] = math.NaN()
	}
	for _, i := range e.extraStr {
		row.Str[i] = ""
	}
}

// fillEntity writes one entity's features from its aggregates:
//
//	mean  = sum_7d / count_7d, missing when count_7d is 0
//	ratio = amount / mean,     missing when mean is missing or not positive
//	seconds_since_first/last:  missing when the key is unseen
//
// A late payment (earlier than the key's latest, which only happens online)
// is measured from the key's latest time, where the state records it, so
// seconds_since_last is 0 rather than negative, a value training never
// sees. Replay feeds payments in order, so offline rows are unaffected.
func (e *Engine) fillEntity(ent Entity, a *Aggregates, t *data.Txn, row schema.Row) {
	s := &e.vel[ent]
	for w := range NumWindows {
		row.Num[s.count[w]] = a.Count[w]
		row.Num[s.sum[w]] = a.Sum[w]
	}
	mean, ratio := math.NaN(), math.NaN()
	if a.Count[weekWindow] > 0 {
		mean = a.Sum[weekWindow] / a.Count[weekWindow]
		if mean > 0 {
			ratio = t.Amount / mean
		}
	}
	row.Num[s.mean] = mean
	row.Num[s.ratio] = ratio
	first, last := math.NaN(), math.NaN()
	if a.Seen {
		now := max(t.DT, a.Last)
		first, last = float64(now-a.First), float64(now-a.Last)
	}
	row.Num[s.sinceFirst] = first
	row.Num[s.sinceLast] = last
	if s.distinct >= 0 {
		row.Num[s.distinct] = a.Distinct
	}
}

// fillMissing marks every feature of an entity whose key is missing.
func (e *Engine) fillMissing(ent Entity, row schema.Row) {
	s := &e.vel[ent]
	nan := math.NaN()
	for w := range NumWindows {
		row.Num[s.count[w]] = nan
		row.Num[s.sum[w]] = nan
	}
	row.Num[s.mean], row.Num[s.ratio] = nan, nan
	row.Num[s.sinceFirst], row.Num[s.sinceLast] = nan, nan
	if s.distinct >= 0 {
		row.Num[s.distinct] = nan
	}
}
