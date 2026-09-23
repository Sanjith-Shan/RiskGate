package features

import (
	"math/rand/v2"
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
