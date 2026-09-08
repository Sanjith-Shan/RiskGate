package features

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestMaxBytesMatchesScalar(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	for range 1000 {
		dst, src := make([]uint8, 64), make([]uint8, 64)
		for i := range dst {
			dst[i], src[i] = uint8(r.IntN(66)), uint8(r.IntN(66))
		}
		want := make([]uint8, 64)
		for i := range want {
			want[i] = max(dst[i], src[i])
		}
		maxBytes(dst, src)
		for i := range want {
			if dst[i] != want[i] {
				t.Fatalf("byte %d: got %d, want %d", i, dst[i], want[i])
			}
		}
	}
}

func TestHLLEstimateSmallCountsAreClose(t *testing.T) {
	for _, n := range []int{0, 1, 2, 5, 20, 200, 5000} {
		regs := make([]uint8, 64)
		r := hllRing{span: 0, size: 64, regs: regs, idx: []int64{0}, cur: 0, width: 1}
		for c := range n {
			r.add([]int{0}, uint64(c)*0x9e3779b97f4a7c15, 6)
		}
		got := r.estimate([]int{0}, 0, 6)
		tol := max(0.5, 0.4*float64(n)) // 64 registers: about 13% standard error
		if d := got - float64(n); d > tol || d < -tol {
			t.Errorf("n=%d: estimate %.2f", n, got)
		}
	}
}

// Regression test for the first/last-seen bug (BUGLOG.md): first and last
// seen were min/max sketches shared by all keys, and a key counted as seen
// when every one of its cells held a time. Once enough other keys had
// filled the table, every never-seen key read as seen, with another key's
// times. On IEEE-CIS that was 392,560 wrong missing values.
func TestSketchNeverSeenKeyReadsUnseen(t *testing.T) {
	s := NewSketch(SketchConfig{Width: 1 << 6, HLLWidth: 16, RecencySlots: 1 << 12})
	for i := range 2000 {
		k, _ := CardKey(float64(i))
		s.Add(k, Event{Time: int64(i), Milli: 1000, Card: NoCard})
	}
	wrong := 0
	for i := range 1000 {
		k, _ := CardKey(float64(1_000_000 + i))
		if s.Read(k, 2000).Seen {
			wrong++
		}
	}
	if wrong > 0 {
		t.Errorf("%d of 1000 never-seen keys read as seen", wrong)
	}
	// Keys that were added still read as seen, with their own times.
	for _, i := range []int{0, 999, 1999} {
		k, _ := CardKey(float64(i))
		if a := s.Read(k, 2000); !a.Seen || a.First != int64(i) || a.Last != int64(i) {
			t.Errorf("key %d: %+v", i, a)
		}
	}
}

// With room for every live key, first and last seen match Exact bit for
// bit, including keys that go idle for the TTL and start over.
func TestSketchRecencyMatchesExact(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	want := replayRows(t, NewExact(testTTL), txns)
	got := replayRows(t, NewSketch(SketchConfig{Width: 1 << 10, HLLWidth: 64, IdleTTL: testTTL}), txns)
	fields := velocityNamesContaining("seconds_since_")
	for i := range want {
		for _, f := range fields {
			s := cat.MustLookup(f).Slot
			g, w := got[i].Num[s], want[i].Num[s]
			if math.Float64bits(g) != math.Float64bits(w) {
				t.Fatalf("row %d %s: sketch %v, exact %v", i, f, g, w)
			}
		}
	}
}

// With far too few slots, the table forgets keys early, and the error is
// one-sided: a feature present in the sketch is present in Exact,
// seconds_since_last is exact, and seconds_since_first never overestimates.
func TestSketchRecencyErrorIsOneSided(t *testing.T) {
	txns := synthetic(t, 20000, 20)
	want := replayRows(t, NewExact(testTTL), txns)
	got := replayRows(t, NewSketch(SketchConfig{Width: 1 << 10, HLLWidth: 64, RecencySlots: 64, IdleTTL: testTTL}), txns)
	forgotten := 0
	for i := range want {
		for _, f := range velocityNamesContaining("seconds_since_") {
			s := cat.MustLookup(f).Slot
			g, w := got[i].Num[s], want[i].Num[s]
			switch {
			case math.IsNaN(g):
				if !math.IsNaN(w) {
					forgotten++
				}
			case math.IsNaN(w):
				t.Fatalf("row %d %s: sketch %v for a key Exact has not seen", i, f, g)
			case strings.HasSuffix(f, "_last") && g != w:
				t.Fatalf("row %d %s: sketch %v, exact %v", i, f, g, w)
			case g > w:
				t.Fatalf("row %d %s: sketch %v overestimates exact %v", i, f, g, w)
			}
		}
	}
	if forgotten == 0 {
		t.Error("64 slots never forgot a key; the test is not exercising eviction")
	}
}
