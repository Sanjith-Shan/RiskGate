package stream

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

func ev(src int32, off int64, ent uint8, dt, id int64) *EntityEvent {
	return &EntityEvent{Src: src, Offset: off, Entity: ent, PaymentID: "txn_" + string(rune('a'+id%26)), Txn: data.Txn{ID: id, DT: dt}}
}

func released(t *testing.T, m *merger) []releaseKey {
	t.Helper()
	var out []releaseKey
	if err := m.release(func(e *EntityEvent) error { out = append(out, keyOf(e)); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMergerHoldsUntilEverySourcePromises: nothing leaves until the minimum
// watermark passes it, and then in event-time order whatever the arrival
// order.
func TestMergerHoldsUntilEverySourcePromises(t *testing.T) {
	m := newMerger(2)
	mustAdd := func(e *EntityEvent) {
		if late, err := m.add(e); err != nil || late {
			t.Fatalf("add %+v: late %v, %v", e, late, err)
		}
	}
	mustAdd(ev(1, 0, 0, 20, 2)) // source 1 runs ahead
	mustAdd(ev(0, 0, 0, 10, 1))
	mustAdd(ev(0, 1, 2, 30, 3))
	if got := released(t, m); len(got) != 0 {
		t.Fatalf("released %v with no watermarks", got)
	}
	_ = m.watermark(Watermark{Src: 0, Pos: Pos{30, 3}})
	if got := released(t, m); len(got) != 0 {
		t.Fatalf("released %v while source 1 promised nothing", got)
	}
	_ = m.watermark(Watermark{Src: 1, Pos: Pos{25, 0}})
	got := released(t, m)
	want := []releaseKey{{Pos{10, 1}, 0, 0, 0}, {Pos{20, 2}, 0, 1, 0}}
	if !slices.Equal(got, want) {
		t.Fatalf("released %v, want %v", got, want)
	}
	_ = m.watermark(Watermark{Src: 1, Pos: MaxPos})
	if got := released(t, m); !slices.Equal(got, []releaseKey{{Pos{30, 3}, 2, 0, 1}}) {
		t.Fatalf("released %v after the final watermark", got)
	}
}

// TestMergerDropsResends: a restarted router re-sends from its committed
// offset; everything up to the highest (offset, entity) seen is a duplicate.
func TestMergerDropsResends(t *testing.T) {
	m := newMerger(1)
	for off := range int64(5) {
		_, _ = m.add(ev(0, off, 0, 100+off, off))
		_, _ = m.add(ev(0, off, 3, 100+off, off))
	}
	for off := int64(2); off < 7; off++ { // resend from offset 2, then two new
		_, _ = m.add(ev(0, off, 0, 100+off, off))
		_, _ = m.add(ev(0, off, 3, 100+off, off))
	}
	if m.dups != 6 {
		t.Fatalf("dropped %d duplicates, want 6", m.dups)
	}
	_ = m.watermark(Watermark{Src: 0, Pos: MaxPos})
	if got := released(t, m); len(got) != 14 {
		t.Fatalf("released %d events, want 14", len(got))
	}
	_ = m.watermark(Watermark{Src: 0, Pos: Pos{1, 1}}) // an old watermark from a restarted router
	if m.wm[0] != MaxPos {
		t.Fatal("a watermark moved backwards")
	}
}

// TestMergerCountsLate: an event at or before the minimum watermark that is
// not a resend breaks the router's promise. It is applied at once and
// counted, never silently dropped.
func TestMergerCountsLate(t *testing.T) {
	m := newMerger(1)
	_ = m.watermark(Watermark{Src: 0, Pos: Pos{50, 5}})
	late, err := m.add(ev(0, 0, 0, 40, 4))
	if err != nil || !late || m.late != 1 {
		t.Fatalf("late %v (count %d), %v", late, m.late, err)
	}
}

// TestMergerRandomInterleavings: any interleaving of in-order sources, with
// resends and watermarks, releases exactly the sorted, deduplicated events.
func TestMergerRandomInterleavings(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for trial := range 200 {
		sources := 1 + r.IntN(4)
		var all []*EntityEvent
		streams := make([][]*EntityEvent, sources)
		id := int64(0)
		dt := int64(0)
		for range 60 {
			dt += r.Int64N(3)
			id++
			s := r.IntN(sources)
			off := int64(len(streams[s]))
			for ent := range uint8(4) {
				if r.IntN(2) == 0 {
					e := ev(int32(s), off, ent, dt, id)
					streams[s] = append(streams[s], e)
					all = append(all, e)
				}
			}
		}
		m := newMerger(sources)
		pos := make([]int, sources)
		var out []releaseKey
		for {
			live := []int{}
			for s := range sources {
				if pos[s] < len(streams[s]) {
					live = append(live, s)
				}
			}
			if len(live) == 0 {
				break
			}
			s := live[r.IntN(len(live))]
			e := streams[s][pos[s]]
			if late, err := m.add(e); err != nil || late {
				t.Fatalf("trial %d: late %v, %v", trial, late, err)
			}
			if r.IntN(5) == 0 && pos[s] > 2 { // a resend of the last few
				for k := pos[s] - 2; k <= pos[s]; k++ {
					_, _ = m.add(streams[s][k])
				}
			}
			pos[s]++
			// A router promises only between payments, never between two
			// entities of one payment.
			endOfPayment := pos[s] == len(streams[s]) || PosOf(&streams[s][pos[s]].Txn) != PosOf(&e.Txn)
			if endOfPayment && r.IntN(3) == 0 {
				// The source promises up to its latest event.
				_ = m.watermark(Watermark{Src: int32(s), Pos: PosOf(&e.Txn)})
				out = append(out, released(t, m)...)
			}
		}
		for s := range sources {
			_ = m.watermark(Watermark{Src: int32(s), Pos: MaxPos})
		}
		out = append(out, released(t, m)...)
		want := make([]releaseKey, len(all))
		for i, e := range all {
			want[i] = keyOf(e)
		}
		slices.SortFunc(want, func(a, b releaseKey) int {
			if a.less(b) {
				return -1
			}
			return 1
		})
		if !slices.Equal(out, want) {
			t.Fatalf("trial %d: released %d events out of order or incomplete, want %d", trial, len(out), len(want))
		}
	}
}

// TestWireRoundTrip: every record kind decodes to what was encoded, NaN
// payloads and signed zeros included.
func TestWireRoundTrip(t *testing.T) {
	tx := data.Txn{ID: 7, DT: -3, Amount: math.Copysign(0, -1), Card1: math.NaN(), ProductCode: "W", DeviceInfo: "Ünïcode", D1: math.Inf(1), IsFraud: -1}
	e := EntityEvent{Src: 3, Offset: 1 << 40, Entity: 2, PaymentID: "txn_7", Produced: 123, Txn: tx}
	got, _, err := decodeAggregatorInput(appendEntityEvent(nil, &e))
	if err != nil || got.Offset != e.Offset || got.PaymentID != e.PaymentID || math.Signbit(got.Txn.Amount) != true || !math.IsNaN(got.Txn.Card1) || got.Txn.DeviceInfo != tx.DeviceInfo {
		t.Fatalf("entity event: %+v, %v", got, err)
	}
	_, wm, err := decodeAggregatorInput(appendWatermark(nil, Watermark{Src: 1, Pos: MaxPos}))
	if err != nil || wm.Pos != MaxPos {
		t.Fatalf("watermark: %+v, %v", wm, err)
	}
	vals := []float64{math.NaN(), math.Copysign(0, -1), 1e-310, math.MaxFloat64}
	_, p, err := decodePart(appendEntityPart(nil, &EntityPart{Agg: 2, Seq: 9, PaymentID: "txn_7", Entity: 1, Values: vals}))
	if err != nil || p.Seq != 9 || len(p.Values) != 4 || math.Float64bits(p.Values[1]) != math.Float64bits(vals[1]) || p.Values[2] != 1e-310 {
		t.Fatalf("entity part: %+v, %v", p, err)
	}
	b, _, err := decodePart(appendBasePart(nil, &BasePart{Src: 1, Offset: 2, PaymentID: "txn_7", Created: 5, Mask: 0b1011, Txn: tx}))
	if err != nil || b.Mask != 0b1011 || b.Txn.ID != 7 {
		t.Fatalf("base part: %+v, %v", b, err)
	}
	if _, _, err := decodePart([]byte{kindEntityPart, 1}); err == nil {
		t.Fatal("truncated part decoded")
	}
}
