package features

import (
	"math"
	"sync"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/data/synth"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

var cat = schema.Default()

// synthetic returns a cached SYNTHETIC dataset. A short span keeps keys
// dense enough that windows, ties, and distinct counts are all exercised.
func synthetic(t testing.TB, rows, days int) []data.Txn {
	t.Helper()
	if raceEnabled {
		rows /= 4
	}
	type key struct{ rows, days int }
	synthMu.Lock()
	defer synthMu.Unlock()
	k := key{rows, days}
	if synthCache[k] == nil {
		txns, _ := synth.Generate(synth.Config{Rows: rows, Days: days, Seed: 42})
		synthCache[k] = txns
	}
	return synthCache[k]
}

var (
	synthMu    sync.Mutex
	synthCache = map[struct{ rows, days int }][]data.Txn{}
)

// oracle recomputes every feature straight from the definitions, in
// O(n * events per key), with none of the engine's incremental machinery.
// widths nil means exact windows (t' > now - W); otherwise bucketed windows
// (floor(t'/w) >= floor(now/w) - W/w), which is what Bucketed promises.
func oracle(t testing.TB, txns []data.Txn, ttl int64, widths *BucketPlan) []schema.Row {
	t.Helper()
	type past struct {
		t     int64
		milli int64
		card  uint64
	}
	type hist struct {
		ev          []past
		streakStart int // first event since the last idle gap
	}
	inWindow := func(w int, tj, now int64) bool {
		if widths == nil {
			return tj > now-windowSeconds[w]
		}
		width := widths[w]
		return floorDiv(tj, width) >= floorDiv(now, width)-windowSeconds[w]/width
	}
	hists := map[Key]*hist{}
	rows := make([]schema.Row, len(txns))
	nan := math.NaN()
	for i := range txns {
		tx := &txns[i]
		row := cat.NewRow()
		set := func(name string, v float64) { row.Num[cat.MustLookup(name).Slot] = v }
		keys, ok := KeysOf(tx)
		for ent := range Entity(NumEntities) {
			p := ent.String() + "_"
			if !ok[ent] {
				continue // everything stays NaN
			}
			h := hists[keys[ent]]
			live := h != nil && len(h.ev) > 0 && tx.DT-h.ev[len(h.ev)-1].t < ttl
			var count [NumWindows]int64
			var milli [NumWindows]int64
			cards := map[uint64]bool{}
			if live {
				// Walk back from the newest event; nothing older than the
				// longest window plus a bucket can be inside any window.
				for j := len(h.ev) - 1; j >= h.streakStart; j-- {
					e := h.ev[j]
					if e.t <= tx.DT-maxWindow-2*86400 {
						break
					}
					for w := range NumWindows {
						if inWindow(w, e.t, tx.DT) {
							count[w]++
							milli[w] += e.milli
							if w == distinctWindow && e.card != NoCard {
								cards[e.card] = true
							}
						}
					}
				}
			}
			for w, win := range schema.Windows {
				set(p+"txn_count_"+win.Name, float64(count[w]))
				set(p+"amount_sum_"+win.Name, float64(milli[w])/1000)
			}
			mean, ratio := nan, nan
			if count[weekWindow] > 0 {
				mean = float64(milli[weekWindow]) / 1000 / float64(count[weekWindow])
				if mean > 0 {
					ratio = tx.Amount / mean
				}
			}
			set(p+"mean_amount_7d", mean)
			set(p+"amount_ratio_7d", ratio)
			first, last := nan, nan
			if live {
				first = float64(tx.DT - h.ev[h.streakStart].t)
				last = float64(tx.DT - h.ev[len(h.ev)-1].t)
			}
			set(p+"seconds_since_first", first)
			set(p+"seconds_since_last", last)
			if ent.tracksDistinct() {
				set("distinct_cards_per_"+ent.String()+"_24h", float64(len(cards)))
			}
		}
		// Raw fields, by their definitions.
		set("amount", tx.Amount)
		set("distance", tx.Dist1)
		set("billing_region", tx.Addr1)
		set("billing_country_code", tx.Addr2)
		for name, v := range map[string]string{
			"product_code": tx.ProductCode, "card_network": tx.Card4, "card_type": tx.Card6,
			"purchaser_email_domain": tx.PEmail, "recipient_email_domain": tx.REmail,
			"device_type": tx.DeviceType, "device_info": tx.DeviceInfo,
		} {
			row.Str[cat.MustLookup(name).Slot] = v
		}
		rows[i] = row

		ev := EventOf(tx)
		for ent := range Entity(NumEntities) {
			if !ok[ent] {
				continue
			}
			h := hists[keys[ent]]
			if h == nil {
				h = &hist{}
				hists[keys[ent]] = h
			}
			if len(h.ev) > 0 && tx.DT-h.ev[len(h.ev)-1].t >= ttl {
				h.streakStart = len(h.ev)
			}
			h.ev = append(h.ev, past{ev.Time, ev.Milli, ev.Card})
		}
	}
	return rows
}

// diffRows reports the first field where two rows differ, comparing floats
// by bit pattern.
func diffRows(a, b schema.Row) (string, bool) {
	for _, f := range cat.Fields() {
		if f.Kind == schema.Number {
			x, y := a.Num[f.Slot], b.Num[f.Slot]
			if math.Float64bits(x) != math.Float64bits(y) {
				return f.Name, true
			}
		} else if a.Str[f.Slot] != b.Str[f.Slot] {
			return f.Name, true
		}
	}
	return "", false
}

func replayRows(t testing.TB, st State, txns []data.Txn) []schema.Row {
	t.Helper()
	eng, err := NewEngine(cat, st)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]schema.Row, 0, len(txns))
	if err := Replay(eng, txns, func(_ *data.Txn, row schema.Row) error {
		rows = append(rows, row.Clone())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func checkAgainstOracle(t *testing.T, got, want []schema.Row, txns []data.Txn) {
	t.Helper()
	bad := 0
	for i := range want {
		if name, differ := diffRows(got[i], want[i]); differ {
			f := cat.MustLookup(name)
			t.Errorf("txn %d (ID %d) %s: got %v, want %v", i, txns[i].ID, name, got[i].Num[f.Slot], want[i].Num[f.Slot])
			if bad++; bad == 10 {
				t.FailNow()
			}
		}
	}
}

// A short idle TTL in these tests makes key expiry and restart common.
const testTTL = 8 * 86400

func TestExactMatchesOracle(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	checkAgainstOracle(t, replayRows(t, NewExact(testTTL), txns), oracle(t, txns, testTTL, nil), txns)
}

func TestBucketedMatchesOracle(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	for _, plan := range []BucketPlan{DefaultBucketPlan, DefaultSketchPlan} {
		st := NewBucketed(BucketedConfig{Plan: plan, DistinctCap: 1 << 20, IdleTTL: testTTL})
		checkAgainstOracle(t, replayRows(t, st, txns), oracle(t, txns, testTTL, &plan), txns)
	}
}

// The oracle is only worth something if the data exercises it.
func TestOracleDataIsDense(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	rows := oracle(t, txns, testTTL, nil)
	slot := func(n string) int { return cat.MustLookup(n).Slot }
	var ties, busy, distinct int
	for i, r := range rows {
		if i > 0 && txns[i].DT == txns[i-1].DT {
			ties++
		}
		if r.Num[slot("device_txn_count_1h")] >= 5 {
			busy++
		}
		if r.Num[slot("distinct_cards_per_device_24h")] >= 5 {
			distinct++
		}
	}
	if ties < 5 || busy < 25 || distinct < 100 {
		t.Errorf("sparse test data: %d ties, %d busy devices, %d multi-card devices", ties, busy, distinct)
	}
}
