package backtest

import "math/bits"

// Bitmap is a fixed-size set of row numbers, one bit per row, packed into
// 64-bit words. Row i is bit i%64 of word i/64. Bits at or past N are always
// zero; every operation that could set them (Not, Fill) clears them again, so
// Count and iteration never see phantom rows.
//
// A dense bitmap is the right shape here: rule masks over a 590K-row table are
// 74 KB each, a whole-table AND is a few thousand word operations, and
// counting uses the hardware popcount. Compressed formats pay off only for
// very sparse sets, and a backtest touches every row anyway.
type Bitmap struct {
	N int
	W []uint64
}

// NewBitmap returns an empty bitmap over n rows.
func NewBitmap(n int) *Bitmap {
	return &Bitmap{N: n, W: make([]uint64, words(n))}
}

// words is the number of 64-bit words needed for n bits.
func words(n int) int { return (n + 63) >> 6 }

// tailMask is the mask of valid bits in the last word of an n-bit bitmap.
func tailMask(n int) uint64 {
	if r := n & 63; r != 0 {
		return 1<<uint(r) - 1
	}
	return ^uint64(0)
}

// Get reports whether row i is set.
func (b *Bitmap) Get(i int) bool { return b.W[i>>6]&(1<<uint(i&63)) != 0 }

// Set adds row i.
func (b *Bitmap) Set(i int) { b.W[i>>6] |= 1 << uint(i&63) }

// Clear removes row i.
func (b *Bitmap) Clear(i int) { b.W[i>>6] &^= 1 << uint(i&63) }

// Clone returns an independent copy.
func (b *Bitmap) Clone() *Bitmap {
	return &Bitmap{N: b.N, W: append([]uint64(nil), b.W...)}
}

// Fill sets every row.
func (b *Bitmap) Fill() *Bitmap {
	for i := range b.W {
		b.W[i] = ^uint64(0)
	}
	b.trim()
	return b
}

// Reset clears every row.
func (b *Bitmap) Reset() *Bitmap {
	clear(b.W)
	return b
}

func (b *Bitmap) trim() {
	if len(b.W) > 0 {
		b.W[len(b.W)-1] &= tailMask(b.N)
	}
}

func (b *Bitmap) mustMatch(o *Bitmap) {
	if b.N != o.N {
		panic("backtest: bitmap sizes differ")
	}
}

// And keeps the rows also in o, in place, and returns b.
func (b *Bitmap) And(o *Bitmap) *Bitmap {
	b.mustMatch(o)
	for i, w := range o.W {
		b.W[i] &= w
	}
	return b
}

// Or adds the rows in o, in place, and returns b.
func (b *Bitmap) Or(o *Bitmap) *Bitmap {
	b.mustMatch(o)
	for i, w := range o.W {
		b.W[i] |= w
	}
	return b
}

// AndNot removes the rows in o, in place, and returns b.
func (b *Bitmap) AndNot(o *Bitmap) *Bitmap {
	b.mustMatch(o)
	for i, w := range o.W {
		b.W[i] &^= w
	}
	return b
}

// Not complements b in place over its N rows and returns b.
func (b *Bitmap) Not() *Bitmap {
	for i := range b.W {
		b.W[i] = ^b.W[i]
	}
	b.trim()
	return b
}

// Count returns the number of rows set.
func (b *Bitmap) Count() int {
	n := 0
	for _, w := range b.W {
		n += bits.OnesCount64(w)
	}
	return n
}

// Any reports whether any row is set.
func (b *Bitmap) Any() bool {
	for _, w := range b.W {
		if w != 0 {
			return true
		}
	}
	return false
}

// AndCount returns |b ∩ o| without allocating.
func (b *Bitmap) AndCount(o *Bitmap) int {
	b.mustMatch(o)
	n := 0
	for i, w := range o.W {
		n += bits.OnesCount64(b.W[i] & w)
	}
	return n
}

// Equal reports whether b and o hold the same rows.
func (b *Bitmap) Equal(o *Bitmap) bool {
	if b.N != o.N {
		return false
	}
	for i, w := range o.W {
		if b.W[i] != w {
			return false
		}
	}
	return true
}

// ForEach calls f with every row set, in increasing order. It strips one set
// bit per step with w &= w-1, so the cost is proportional to the number of
// rows set, not to N.
func (b *Bitmap) ForEach(f func(row int)) {
	for i, w := range b.W {
		for w != 0 {
			f(i<<6 | bits.TrailingZeros64(w))
			w &= w - 1
		}
	}
}

// Next returns the first row set at or after i, or -1 if there is none.
func (b *Bitmap) Next(i int) int {
	if i < 0 {
		i = 0
	}
	if i >= b.N {
		return -1
	}
	wi := i >> 6
	w := b.W[wi] &^ (1<<uint(i&63) - 1)
	for {
		if w != 0 {
			return wi<<6 | bits.TrailingZeros64(w)
		}
		wi++
		if wi == len(b.W) {
			return -1
		}
		w = b.W[wi]
	}
}

// Rows returns the rows set, in order. For small results and tests.
func (b *Bitmap) Rows() []int {
	out := make([]int, 0, b.Count())
	b.ForEach(func(r int) { out = append(out, r) })
	return out
}
