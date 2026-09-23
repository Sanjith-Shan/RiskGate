package backtest

import (
	"math"
	"math/bits"
	"strings"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
)

// The column operators. A number node returns one float64 per row of the
// chunk; a condition node writes one bit per row into words(c.n) words, and
// leaves the bits past c.n in the last word zero, so a chunk's output can be
// stored straight into the result bitmap.
//
// The kernels are written out per operator rather than parameterized by a
// function value: a call per row would cost more than the comparison
// itself. Each loop body is a single operation stored to memory, which also
// keeps the compiler from fusing a multiply and an add into one FMA
// instruction; the closure evaluator rounds after every step, and a fused
// result could differ from it in the last bit.

type numNode interface {
	eval(c *chunk) []float64
}

type boolNode interface {
	eval(c *chunk, out []uint64)
}

// b2u converts a condition to a bit without a branch (CSET on arm64, SETcc
// on amd64).
func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// ---- numbers -----------------------------------------------------------

type colNum struct{ col []float64 }

func (n colNum) eval(c *chunk) []float64 { return n.col[c.lo : c.lo+c.n] }

type negNum struct {
	x   numNode
	buf int
}

func (n negNum) eval(c *chunk) []float64 {
	x := n.x.eval(c)
	out := c.s.f[n.buf][:len(x)]
	for i, v := range x {
		out[i] = -v
	}
	return out
}

type arithVV struct {
	op   rules.Op
	x, y numNode
	buf  int
}

func (n arithVV) eval(c *chunk) []float64 {
	x, y := n.x.eval(c), n.y.eval(c)
	out := c.s.f[n.buf][:len(x)]
	y = y[:len(x)]
	switch n.op {
	case rules.OpAdd:
		for i := range out {
			out[i] = x[i] + y[i]
		}
	case rules.OpSub:
		for i := range out {
			out[i] = x[i] - y[i]
		}
	case rules.OpMul:
		for i := range out {
			out[i] = x[i] * y[i]
		}
	case rules.OpDiv:
		for i := range out {
			out[i] = div(x[i], y[i])
		}
	}
	return out
}

type arithVC struct {
	op  rules.Op
	x   numNode
	c   float64
	buf int
}

func (n arithVC) eval(c *chunk) []float64 {
	x := n.x.eval(c)
	out := c.s.f[n.buf][:len(x)]
	k := n.c
	switch n.op {
	case rules.OpAdd:
		for i, v := range x {
			out[i] = v + k
		}
	case rules.OpSub:
		for i, v := range x {
			out[i] = v - k
		}
	case rules.OpMul:
		for i, v := range x {
			out[i] = v * k
		}
	case rules.OpDiv:
		if k == 0 {
			for i := range out {
				out[i] = math.NaN()
			}
			break
		}
		for i, v := range x {
			out[i] = v / k
		}
	}
	return out
}

type arithCV struct {
	op  rules.Op
	c   float64
	y   numNode
	buf int
}

func (n arithCV) eval(c *chunk) []float64 {
	y := n.y.eval(c)
	out := c.s.f[n.buf][:len(y)]
	k := n.c
	switch n.op {
	case rules.OpAdd:
		for i, v := range y {
			out[i] = k + v
		}
	case rules.OpSub:
		for i, v := range y {
			out[i] = k - v
		}
	case rules.OpMul:
		for i, v := range y {
			out[i] = k * v
		}
	case rules.OpDiv:
		for i, v := range y {
			out[i] = div(k, v)
		}
	}
	return out
}

func div(a, b float64) float64 {
	if b == 0 {
		return math.NaN()
	}
	return a / b
}

// ---- conditions --------------------------------------------------------

type constBool bool

func (b constBool) eval(c *chunk, out []uint64) {
	if !b {
		clear(out)
		return
	}
	for i := range out {
		out[i] = ^uint64(0)
	}
	out[len(out)-1] &= tailMask(c.n)
}

type notNode struct{ x boolNode }

func (n notNode) eval(c *chunk, out []uint64) {
	n.x.eval(c, out)
	for i := range out {
		out[i] = ^out[i]
	}
	out[len(out)-1] &= tailMask(c.n)
}

// logicNode is and/or. It skips its right operand when the left one already
// decides the whole chunk, which is common: a selective first conjunct
// leaves most chunks empty.
type logicNode struct {
	or   bool
	x, y boolNode
	buf  int
}

func (n logicNode) eval(c *chunk, out []uint64) {
	n.x.eval(c, out)
	if n.or {
		if allSet(out, c.n) {
			return
		}
	} else if allZero(out) {
		return
	}
	if !n.or && sparse(out, c.n) {
		if r, ok := n.y.(refiner); ok && r.refine(c, out) {
			return
		}
	}
	tmp := c.s.w[n.buf][:len(out)]
	n.y.eval(c, tmp)
	if n.or {
		for i, w := range tmp {
			out[i] |= w
		}
	} else {
		for i, w := range tmp {
			out[i] &= w
		}
	}
}

// refine evaluates an and-node's right side only on the rows its left
// side left standing, when those are few: like a selection vector in a
// column store, and like the short circuit the row-at-a-time evaluator gets
// for free. Real rules are mostly conjunctions whose first test is
// selective (an email domain, a product code, a high score), so after it
// most chunks hold a few dozen candidate rows, and testing those beats
// testing all 4096.
func (n logicNode) refine(c *chunk, sel []uint64) bool {
	if n.or {
		return false
	}
	for _, side := range [2]boolNode{n.x, n.y} {
		r, ok := side.(refiner)
		if !ok || !r.refine(c, sel) {
			// Fall back to evaluating the side in full into scratch.
			tmp := c.s.w[n.buf][:len(sel)]
			side.eval(c, tmp)
			for i, w := range tmp {
				sel[i] &= w
			}
		}
		if allZero(sel) {
			return true
		}
	}
	return true
}

// sparseDensity is the fraction of candidate rows below which refining a
// chunk beats evaluating it in full. Refinement pays a bit scan and a
// random access per row against the full kernels' streaming loop, so it
// wins only well below one row in eight.
const sparseDensity = 16

func sparse(ws []uint64, n int) bool {
	set := 0
	for _, w := range ws {
		set += bits.OnesCount64(w)
	}
	return set*sparseDensity <= n
}

// refiner is a condition that can clear, in sel, the rows where it is
// false, looking only at the rows set in sel. refine reports false if it
// cannot, and then sel is untouched.
type refiner interface {
	refine(c *chunk, sel []uint64) bool
}

// eachSet calls keep for every row set in sel (as an offset in the chunk)
// and clears the rows for which it returns false.
func eachSet(sel []uint64, keep func(i int) bool) {
	for w, word := range sel {
		for x := word; x != 0; x &= x - 1 {
			j := bits.TrailingZeros64(x)
			if !keep(w<<6 | j) {
				sel[w] &^= 1 << uint(j)
			}
		}
	}
}

func (n cmpVC) refine(c *chunk, sel []uint64) bool {
	col, ok := n.x.(colNum)
	if !ok {
		return false
	}
	x, k, op := col.col[c.lo:c.lo+c.n], n.c, n.op
	eachSet(sel, func(i int) bool { return compare(op, x[i], k) })
	return true
}

func (n cmpVV) refine(c *chunk, sel []uint64) bool {
	cx, okx := n.x.(colNum)
	cy, oky := n.y.(colNum)
	if !okx || !oky {
		return false
	}
	x, y, op := cx.col[c.lo:c.lo+c.n], cy.col[c.lo:c.lo+c.n], n.op
	eachSet(sel, func(i int) bool { return compare(op, x[i], y[i]) })
	return true
}

func (n isNaNNode) refine(c *chunk, sel []uint64) bool {
	col, ok := n.x.(colNum)
	if !ok {
		return false
	}
	x := col.col[c.lo : c.lo+c.n]
	eachSet(sel, func(i int) bool { return x[i] != x[i] })
	return true
}

func (n inNumNode) refine(c *chunk, sel []uint64) bool {
	col, ok := n.x.(colNum)
	if !ok {
		return false
	}
	x, set := col.col[c.lo:c.lo+c.n], n.set
	eachSet(sel, func(i int) bool { return containsNum(set, x[i]) })
	return true
}

func (n lutNode) refine(c *chunk, sel []uint64) bool {
	codes, lut := n.codes[c.lo:c.lo+c.n], n.lut
	eachSet(sel, func(i int) bool { return lut[codes[i]] != 0 })
	return true
}

func (n strPair) refine(c *chunk, sel []uint64) bool {
	a, b := n.a[c.lo:c.lo+c.n], n.b[c.lo:c.lo+c.n]
	eachSet(sel, func(i int) bool {
		x, y := n.ida[a[i]], n.idb[b[i]]
		return x != 0 && y != 0 && (x == y) != n.ne
	})
	return true
}

func allZero(ws []uint64) bool {
	for _, w := range ws {
		if w != 0 {
			return false
		}
	}
	return true
}

func allSet(ws []uint64, n int) bool {
	last := len(ws) - 1
	for _, w := range ws[:last] {
		if w != ^uint64(0) {
			return false
		}
	}
	return ws[last] == tailMask(n)
}

// cmpVC compares a vector with a constant that is not NaN.
type cmpVC struct {
	op rules.Op
	x  numNode
	c  float64
}

func (n cmpVC) eval(c *chunk, out []uint64) {
	x := n.x.eval(c)
	k := n.c
	for w := range out {
		xs := x[w<<6 : min(len(x), w<<6+64)]
		var word uint64
		switch n.op {
		case rules.OpEq:
			for j, v := range xs {
				word |= b2u(v == k) << uint(j)
			}
		case rules.OpNe:
			// k is not NaN, so only v can be missing: v == v rules it out.
			for j, v := range xs {
				word |= b2u(v != k && v == v) << uint(j)
			}
		case rules.OpLt:
			for j, v := range xs {
				word |= b2u(v < k) << uint(j)
			}
		case rules.OpLe:
			for j, v := range xs {
				word |= b2u(v <= k) << uint(j)
			}
		case rules.OpGt:
			for j, v := range xs {
				word |= b2u(v > k) << uint(j)
			}
		case rules.OpGe:
			for j, v := range xs {
				word |= b2u(v >= k) << uint(j)
			}
		}
		out[w] = word
	}
}

type cmpVV struct {
	op   rules.Op
	x, y numNode
}

func (n cmpVV) eval(c *chunk, out []uint64) {
	x, y := n.x.eval(c), n.y.eval(c)
	for w := range out {
		lo, hi := w<<6, min(len(x), w<<6+64)
		xs, ys := x[lo:hi], y[lo:hi]
		ys = ys[:len(xs)]
		var word uint64
		switch n.op {
		case rules.OpEq:
			for j, a := range xs {
				word |= b2u(a == ys[j]) << uint(j)
			}
		case rules.OpNe:
			for j, a := range xs {
				b := ys[j]
				word |= b2u(a == a && b == b && a != b) << uint(j)
			}
		case rules.OpLt:
			for j, a := range xs {
				word |= b2u(a < ys[j]) << uint(j)
			}
		case rules.OpLe:
			for j, a := range xs {
				word |= b2u(a <= ys[j]) << uint(j)
			}
		case rules.OpGt:
			for j, a := range xs {
				word |= b2u(a > ys[j]) << uint(j)
			}
		case rules.OpGe:
			for j, a := range xs {
				word |= b2u(a >= ys[j]) << uint(j)
			}
		}
		out[w] = word
	}
}

type isNaNNode struct{ x numNode }

func (n isNaNNode) eval(c *chunk, out []uint64) {
	x := n.x.eval(c)
	for w := range out {
		var word uint64
		for j, v := range x[w<<6 : min(len(x), w<<6+64)] {
			word |= b2u(v != v) << uint(j)
		}
		out[w] = word
	}
}

// inNumNode tests membership in a sorted list: a short list is scanned, a
// long one binary-searched.
type inNumNode struct {
	x   numNode
	set []float64
}

func (n inNumNode) eval(c *chunk, out []uint64) {
	x := n.x.eval(c)
	set := n.set
	for w := range out {
		var word uint64
		for j, v := range x[w<<6 : min(len(x), w<<6+64)] {
			var hit bool
			if len(set) <= 8 {
				for _, s := range set {
					hit = hit || v == s
				}
			} else {
				hit = containsNum(set, v)
			}
			word |= b2u(hit) << uint(j)
		}
		out[w] = word
	}
}

// lutNode decides a string predicate per row by looking up the row's
// dictionary code in a table computed once per distinct value.
type lutNode struct {
	codes []uint32
	lut   []uint8
}

func (n lutNode) eval(c *chunk, out []uint64) {
	codes := n.codes[c.lo : c.lo+c.n]
	lut := n.lut
	for w := range out {
		var word uint64
		for j, code := range codes[w<<6 : min(len(codes), w<<6+64)] {
			word |= uint64(lut[code]) << uint(j)
		}
		out[w] = word
	}
}

// strPair compares two string columns through ids shared by both views.
type strPair struct {
	ne       bool
	a, b     []uint32 // codes
	ida, idb []uint32 // code -> shared id; 0 is missing
}

func (n strPair) eval(c *chunk, out []uint64) {
	a, b := n.a[c.lo:c.lo+c.n], n.b[c.lo:c.lo+c.n]
	for w := range out {
		lo, hi := w<<6, min(len(a), w<<6+64)
		var word uint64
		for j := range hi - lo {
			x, y := n.ida[a[lo+j]], n.idb[b[lo+j]]
			word |= b2u(x != 0 && y != 0 && (x == y) != n.ne) << uint(j)
		}
		out[w] = word
	}
}

// prefixPair is starts_with over two string columns. It is the one string
// operator that works on text per row; it only arises from rules comparing
// two attributes, which real rules almost never do.
type prefixPair struct {
	a, b   []uint32
	va, vb []string
}

func (n prefixPair) eval(c *chunk, out []uint64) {
	a, b := n.a[c.lo:c.lo+c.n], n.b[c.lo:c.lo+c.n]
	for w := range out {
		lo, hi := w<<6, min(len(a), w<<6+64)
		var word uint64
		for j := range hi - lo {
			x, y := n.va[a[lo+j]], n.vb[b[lo+j]]
			word |= b2u(x != "" && y != "" && strings.HasPrefix(x, y)) << uint(j)
		}
		out[w] = word
	}
}
