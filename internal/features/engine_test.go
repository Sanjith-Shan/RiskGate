package features

import (
	"math"
	"strings"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// stateKinds builds one of each implementation, with configurations small
// enough for tests.
var stateKinds = []struct {
	name  string
	new   func() State
	exact bool // matches the exact definitions bit for bit
}{
	{"exact", func() State { return NewExact(0) }, true},
	{"bucketed", func() State { return NewBucketed(BucketedConfig{}) }, false},
	{"sketch", func() State { return NewSketch(SketchConfig{Width: 1 << 12, HLLWidth: 256}) }, false},
	{"sharded-exact", func() State { return NewSharded(4, func() State { return NewExact(0) }) }, true},
	{"locked-bucketed", func() State { return NewLocked(func() State { return NewBucketed(BucketedConfig{}) }) }, false},
	{"syncmap-exact", func() State { return NewSyncMap(NewExact(0)) }, true},
	{"syncmap-bucketed", func() State { return NewSyncMap(NewBucketed(BucketedConfig{})) }, false},
}

func newEngine(t testing.TB, st State) *Engine {
	t.Helper()
	e, err := NewEngine(cat, st)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func num(row schema.Row, name string) float64 { return row.Num[cat.MustLookup(name).Slot] }

// txn builds a payment with every key present.
func txn(id, dt int64, amount, card1 float64) data.Txn {
	return data.Txn{ID: id, DT: dt, Amount: amount, Card1: card1, Addr1: 300, Addr2: 87, D1: 0,
		Dist1: math.NaN(), DeviceInfo: "dev", PEmail: "mail.com", ProductCode: "W"}
}

func replayAll(t testing.TB, e *Engine, txns []data.Txn) []schema.Row {
	t.Helper()
	var rows []schema.Row
	if err := Replay(e, txns, func(_ *data.Txn, r schema.Row) error {
		rows = append(rows, r.Clone())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestWindowEdges pins the boundary: an event exactly W seconds before now
// is outside window W, one second later is inside. Bucketed may also count
// the older one (its documented overcount); nothing may drop the newer one.
func TestWindowEdges(t *testing.T) {
	const now = 10 * 86400
	for _, win := range schema.Windows {
		W := win.Seconds
		txns := []data.Txn{
			txn(1, now-W-1, 1, 7),
			txn(2, now-W, 10, 7),
			txn(3, now-W+1, 100, 7),
			txn(4, now, 1000, 7),
		}
		for _, k := range stateKinds {
			rows := replayAll(t, newEngine(t, k.new()), txns)
			count := num(rows[3], "card_txn_count_"+win.Name)
			sum := num(rows[3], "card_amount_sum_"+win.Name)
			if k.exact {
				if count != 1 || sum != 100 {
					t.Errorf("%s window %s: got count %v sum %v, want 1 and 100", k.name, win.Name, count, sum)
				}
				continue
			}
			// Approximations may overcount by up to a bucket, never undercount.
			if count < 1 || sum < 100 || count > 3 {
				t.Errorf("%s window %s: got count %v sum %v", k.name, win.Name, count, sum)
			}
		}
	}
}

// Bucketed's overcount is exactly the oldest bucket: with 60s buckets at
// now = 7200 the hour window spans buckets 60..120, so t=3600 (exactly an
// hour old, excluded by Exact) is counted and t=3599 is not.
func TestBucketedEdgeIsOneBucket(t *testing.T) {
	st := NewBucketed(BucketedConfig{})
	k, _ := CardKey(1)
	for _, tm := range []int64{3599, 3600, 3659} {
		st.Add(k, Event{Time: tm, Milli: 1000, Card: NoCard})
	}
	if a := st.Read(k, 7200); a.Count[0] != 2 {
		t.Errorf("1h count at 7200: got %v, want 2 (buckets 60 and later)", a.Count[0])
	}
	if a := st.Read(k, 7260); a.Count[0] != 0 {
		t.Errorf("1h count at 7260: got %v, want 0", a.Count[0])
	}
}

// Payments at the same second are ordered by TransactionID: the lower ID
// is earlier and is seen by the higher one, never the reverse.
func TestTiesBreakByTransactionID(t *testing.T) {
	txns := []data.Txn{txn(10, 5000, 1, 7), txn(11, 5000, 2, 7), txn(12, 5000, 4, 7)}
	for _, k := range stateKinds {
		rows := replayAll(t, newEngine(t, k.new()), txns)
		for i, want := range []float64{0, 1, 2} {
			if got := num(rows[i], "card_txn_count_1h"); got != want {
				t.Errorf("%s: ID %d sees %v earlier payments, want %v", k.name, txns[i].ID, got, want)
			}
		}
		if got := num(rows[2], "card_seconds_since_last"); got != 0 {
			t.Errorf("%s: seconds since last at a tie: got %v, want 0", k.name, got)
		}
	}
	// And the driver refuses input that is not in that order.
	swapped := []data.Txn{txns[1], txns[0]}
	if err := Replay(newEngine(t, NewExact(0)), swapped, nil); err == nil {
		t.Error("Replay accepted a tie out of ID order")
	}
	if err := Replay(newEngine(t, NewExact(0)), []data.Txn{txns[0], txns[0]}, nil); err == nil {
		t.Error("Replay accepted a duplicate")
	}
}

func TestMissingAndUnseenKeys(t *testing.T) {
	e := newEngine(t, NewExact(0))
	a := txn(1, 1000, 50, 7)
	a.DeviceInfo, a.D1 = "", math.NaN() // no device, no uid
	b := txn(2, 2000, 20, 7)
	b.DeviceInfo, b.D1 = "", math.NaN()
	rows := replayAll(t, e, []data.Txn{a, b})

	for _, name := range []string{
		"device_txn_count_1h", "device_amount_sum_7d", "device_mean_amount_7d", "device_seconds_since_first",
		"distinct_cards_per_device_24h", "uid_txn_count_24h", "uid_amount_ratio_7d",
	} {
		if v := num(rows[1], name); !math.IsNaN(v) {
			t.Errorf("%s with the key missing: got %v, want missing", name, v)
		}
	}
	// A present but never-seen key: zero counts, missing mean and times.
	first := rows[0]
	for name, want := range map[string]float64{
		"card_txn_count_1h": 0, "card_amount_sum_7d": 0, "distinct_cards_per_email_24h": 0,
	} {
		if v := num(first, name); v != want {
			t.Errorf("%s for a new key: got %v, want %v", name, v, want)
		}
	}
	for _, name := range []string{"card_mean_amount_7d", "card_amount_ratio_7d", "card_seconds_since_first", "card_seconds_since_last"} {
		if v := num(first, name); !math.IsNaN(v) {
			t.Errorf("%s for a new key: got %v, want missing", name, v)
		}
	}
	// The second payment sees the first.
	for name, want := range map[string]float64{
		"card_txn_count_1h": 1, "card_amount_sum_1h": 50, "card_mean_amount_7d": 50,
		"card_amount_ratio_7d": 0.4, "card_seconds_since_first": 1000, "card_seconds_since_last": 1000,
		"email_txn_count_24h": 1, "distinct_cards_per_email_24h": 1,
	} {
		if v := num(rows[1], name); v != want {
			t.Errorf("%s: got %v, want %v", name, v, want)
		}
	}
	if v := num(rows[1], "risk_score"); !math.IsNaN(v) {
		t.Errorf("risk_score: got %v, want NaN (the model fills it)", v)
	}
}

func TestDistinctCards(t *testing.T) {
	var txns []data.Txn
	cards := []float64{1, 2, 1, 3, 2, 1}
	for i, c := range cards {
		txns = append(txns, txn(int64(i+1), int64(1000+60*i), 5, c))
	}
	late := txn(99, 1000+86400+300, 5, 9) // a day later: only the last few are inside 24h
	txns = append(txns, late)
	for _, k := range stateKinds {
		rows := replayAll(t, newEngine(t, k.new()), txns)
		for i, want := range []float64{0, 1, 2, 2, 3, 3} {
			got := num(rows[i], "distinct_cards_per_device_24h")
			if k.name == "sketch" && math.Abs(got-want) < 0.5 {
				continue // HyperLogLog estimates; small counts land close
			}
			if got != want {
				t.Errorf("%s: payment %d sees %v distinct cards, want %v", k.name, i, got, want)
			}
		}
		// The late payment is at 87700, so the exact 24h window holds only
		// t > 1300: the newest earlier payment, at 1300, is exactly a day old
		// and out. Bucketed and the sketch may still count the oldest bucket.
		got := num(rows[6], "distinct_cards_per_device_24h")
		if k.exact && got != 0 {
			t.Errorf("%s: late payment sees %v distinct cards, want 0", k.name, got)
		}
		if got < 0 || got > 3.5 { // 3 plus HyperLogLog slack
			t.Errorf("%s: late payment sees %v distinct cards", k.name, got)
		}
	}
}

func TestBucketedDistinctSaturates(t *testing.T) {
	st := NewBucketed(BucketedConfig{DistinctCap: 5})
	k, _ := DeviceKey("farm")
	for i := range 50 {
		st.Add(k, Event{Time: int64(i), Milli: 1000, Card: uint64(i)})
	}
	if a := st.Read(k, 60); a.Distinct != 5 || a.Count[0] != 50 {
		t.Errorf("got distinct %v count %v, want the cap 5 and all 50 payments", a.Distinct, a.Count[0])
	}
	// A day later the cards have left the window and tracking resumes.
	st.Add(k, Event{Time: 2 * 86400, Milli: 1000, Card: 777})
	if a := st.Read(k, 2*86400+1); a.Distinct != 1 {
		t.Errorf("after the window passed: got %v distinct, want 1", a.Distinct)
	}
}

func TestIdleTTL(t *testing.T) {
	const ttl = 8 * 86400
	for _, st := range []State{NewExact(ttl), NewBucketed(BucketedConfig{IdleTTL: ttl}), NewSyncMap(NewExact(ttl))} {
		k, _ := CardKey(3)
		st.Add(k, Event{Time: 0, Milli: 1000, Card: NoCard})
		if a := st.Read(k, ttl-1); !a.Seen || a.First != 0 {
			t.Errorf("%T: key forgotten before its TTL: %+v", st, a)
		}
		if a := st.Read(k, ttl); a.Seen {
			t.Errorf("%T: key remembered at its TTL", st)
		}
		// The next payment starts it over.
		st.Add(k, Event{Time: ttl + 5, Milli: 1000, Card: NoCard})
		if a := st.Read(k, ttl+6); a.First != ttl+5 || a.Count[2] != 1 {
			t.Errorf("%T: after restart got %+v", st, a)
		}
		// Physical eviction frees the key without changing any answer.
		before := st.Read(k, 3*ttl)
		if n := st.EvictIdle(3 * ttl); n != 1 {
			t.Errorf("%T: evicted %d keys, want 1", st, n)
		}
		if after := st.Read(k, 3*ttl); after != before && !(math.IsNaN(after.Distinct) && math.IsNaN(before.Distinct)) {
			t.Errorf("%T: eviction changed a read: %+v then %+v", st, before, after)
		}
		if s := st.Stats(); s.Keys != 0 {
			t.Errorf("%T: %d keys after eviction", st, s.Keys)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("a TTL shorter than 7d was accepted")
		}
	}()
	NewExact(86400)
}

// A late event (earlier than the key's latest) is recorded at the latest
// time, so it is counted and never breaks the deque's time order.
func TestLateEventIsClamped(t *testing.T) {
	for _, k := range stateKinds {
		st := k.new()
		key, _ := CardKey(5)
		st.Add(key, Event{Time: 10_000, Milli: 1000, Card: NoCard})
		st.Add(key, Event{Time: 9_000, Milli: 2000, Card: NoCard})
		a := st.Read(key, 10_000+3599)
		if a.Count[0] != 2 || a.Sum[0] != 3 || a.Last != 10_000 {
			t.Errorf("%s: got %+v", k.name, a)
		}
	}
}

// A late payment is scored at the time it is recorded, the key's latest,
// so its features are those of a payment at that time: never a negative
// seconds_since_last, which training never sees, and no window counting an
// event after the payment's own time.
func TestLateEventIsScoredAtKeyLatest(t *testing.T) {
	txns := []data.Txn{txn(1, 1000, 1, 7), txn(2, 5000, 2, 7), txn(3, 4000, 4, 7)}
	for _, k := range stateKinds {
		e := newEngine(t, k.new())
		row := e.NewRow()
		for i := range txns {
			e.ScoreAndUpdate(&txns[i], row)
		}
		if last, first := num(row, "card_seconds_since_last"), num(row, "card_seconds_since_first"); last != 0 || first != 4000 {
			t.Errorf("%s: seconds since last %v, first %v; want 0 and 4000", k.name, last, first)
		}
		if got := num(row, "card_txn_count_1h"); k.exact && got != 1 {
			t.Errorf("%s: 1h count %v, want 1 (the payment at 1000 is over an hour before 5000)", k.name, got)
		}
	}
}

func TestScoreThenUpdateEqualsScoreAndUpdate(t *testing.T) {
	txns := synthetic(t, 5000, 5)
	for _, k := range stateKinds {
		a, b := newEngine(t, k.new()), newEngine(t, k.new())
		ra, rb := a.NewRow(), b.NewRow()
		for i := range txns {
			a.ScoreInto(&txns[i], ra)
			a.Update(&txns[i])
			b.ScoreAndUpdate(&txns[i], rb)
			if name, differ := diffRows(ra, rb); differ {
				t.Fatalf("%s: txn %d %s differs", k.name, i, name)
			}
		}
		if s := a.Score(&txns[0]); len(s.Num) != cat.NumCount() {
			t.Errorf("Score returned a %d-wide row", len(s.Num))
		}
	}
}

func TestScoreIntoDoesNotAllocate(t *testing.T) {
	txns := synthetic(t, 5000, 5)
	for _, k := range stateKinds {
		e := newEngine(t, k.new())
		if err := Replay(e, txns, nil); err != nil {
			t.Fatal(err)
		}
		row := e.NewRow()
		i := 0
		allocs := testing.AllocsPerRun(200, func() {
			e.ScoreInto(&txns[i%len(txns)], row)
			i += 37
		})
		if allocs != 0 {
			t.Errorf("%s: %v allocations per ScoreInto", k.name, allocs)
		}
	}
}

func TestNewEngineChecksCatalog(t *testing.T) {
	small := schema.New([]schema.Field{{Name: "amount", Kind: schema.Number}})
	if _, err := NewEngine(small, NewExact(0)); err == nil || !strings.Contains(err.Error(), "no field") {
		t.Errorf("got %v, want a missing-field error", err)
	}
	fields := append([]schema.Field{}, cat.Fields()...)
	for i := range fields {
		if fields[i].Name == "amount" {
			fields[i].Kind = schema.String
		}
	}
	if _, err := NewEngine(schema.New(fields), NewExact(0)); err == nil || !strings.Contains(err.Error(), "is a string") {
		t.Errorf("got %v, want a kind error", err)
	}
	// Extra fields are tolerated and left missing.
	extra := schema.New(append(append([]schema.Field{}, cat.Fields()...),
		schema.Field{Name: "x_num", Kind: schema.Number}, schema.Field{Name: "x_str", Kind: schema.String}))
	e, err := NewEngine(extra, NewExact(0))
	if err != nil {
		t.Fatal(err)
	}
	row := e.NewRow()
	row.Str[extra.MustLookup("x_str").Slot] = "stale"
	row.Num[extra.MustLookup("x_num").Slot] = 1
	tx := txn(1, 1, 1, 1)
	e.ScoreInto(&tx, row)
	if row.Str[extra.MustLookup("x_str").Slot] != "" || !math.IsNaN(row.Num[extra.MustLookup("x_num").Slot]) {
		t.Error("unknown catalog fields were not reset to missing")
	}
}

// The sketch may only overestimate counts and sums (count-min never
// undercounts), and its window edge is the bucketed one.
func TestSketchNeverUndercounts(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	plan := DefaultSketchPlan
	floor := oracle(t, txns, testTTL, &plan)
	got := replayRows(t, NewSketch(SketchConfig{Width: 1 << 14, IdleTTL: testTTL}), txns)
	exact := 0
	for i := range got {
		for _, f := range velocityNamesContaining("txn_count_", "amount_sum_") {
			s := cat.MustLookup(f).Slot
			g, w := got[i].Num[s], floor[i].Num[s]
			if math.IsNaN(w) {
				if !math.IsNaN(g) {
					t.Fatalf("row %d %s: got %v for a missing key", i, f, g)
				}
				continue
			}
			if g < w-1e-9 {
				t.Fatalf("row %d %s: sketch %v below the bucketed truth %v", i, f, g, w)
			}
			if g == w {
				exact++
			}
		}
	}
	if exact == 0 {
		t.Error("the sketch was never exact; the collision rate is implausible")
	}
}

// velocityNamesContaining lists velocity feature names containing any of
// the given parts.
func velocityNamesContaining(parts ...string) []string {
	var out []string
	for _, f := range schema.VelocityFieldNames() {
		for _, p := range parts {
			if strings.Contains(f.Name, p) {
				out = append(out, f.Name)
				break
			}
		}
	}
	return out
}

func TestCompareStates(t *testing.T) {
	txns := synthetic(t, 5000, 5)
	same, err := CompareStates(cat, txns, NewExact(0), NewExact(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range same {
		if f.ExactFraction() != 1 || f.MissingMismatch != 0 || f.MaxAbs != 0 {
			t.Errorf("exact vs exact: %+v", f)
		}
	}
	diff, err := CompareStates(cat, txns, NewExact(0), NewBucketed(BucketedConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	var over int
	for _, f := range diff {
		if strings.Contains(f.Name, "_txn_count_") && f.Under != 0 {
			t.Errorf("%s: bucketed undercounted %d times", f.Name, f.Under)
		}
		over += f.Over
	}
	if over == 0 {
		t.Error("bucketed never differed from exact; the comparison is not measuring anything")
	}
}

const keyHashGolden = 0x7bd7cd961b67150b

func TestKeys(t *testing.T) {
	if _, ok := CardKey(math.NaN()); ok {
		t.Error("NaN card1 made a key")
	}
	a, _ := CardKey(0)
	b, _ := CardKey(math.Copysign(0, -1))
	if a != b || a.Hash() != b.Hash() {
		t.Error("-0 and +0 make different keys")
	}
	for _, missing := range [][3]float64{{math.NaN(), 1, 1}, {1, math.NaN(), 1}, {1, 1, math.NaN()}} {
		if _, ok := UIDKey(missing[0], missing[1], 86400, missing[2]); ok {
			t.Errorf("uid from %v should be missing", missing)
		}
	}
	// The uid is stable for one customer: a day later D1 is one higher.
	u1, _ := UIDKey(5001, 210, 5*86400+100, 14)
	u2, _ := UIDKey(5001, 210, 6*86400+50000, 15)
	if u1 != u2 {
		t.Errorf("uid moved: %v then %v", u1, u2)
	}
	if u1.String() != "uid:5001/210/-9" {
		t.Errorf("String: %q", u1.String())
	}
	d, _ := DeviceKey("TestPhone X1")
	if d.String() != `device:"TestPhone X1"` || Entity(9).String() != "Entity(9)" {
		t.Errorf("String: %q", d.String())
	}
	// Golden hash: sketch cells and shard assignment are persisted in
	// snapshots, so the hash must never change silently.
	if h := a.Hash(); h != keyHashGolden {
		t.Errorf("CardKey(0).Hash() = %#x, want %#x; snapshots from older builds would be misread", h, keyHashGolden)
	}
}
