package backtest

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// This file is the second back end of the rule compiler: a checked rule
// condition becomes a tree of column operators that evaluates a block of
// rows at a time into a bitmap. The first back end (rules.Compile) turns the
// same AST into closures over one schema.Row, for the online path. The two
// share nothing but the AST and the documented semantics in rules/doc.go,
// which is what makes the differential test between them meaningful.
//
// Evaluation is chunked: rows are processed chunkRows at a time, so every
// intermediate vector (4096 float64s = 32 KB) stays in L1/L2 cache while the
// operators above it read it, and the scratch buffers are reused from chunk
// to chunk instead of materializing whole-table intermediates. Chunks start
// on 64-row boundaries, so each chunk owns whole words of the output bitmap
// and chunks can be evaluated by different goroutines with no locking.
//
// Representation by type:
//
//   - number: a []float64 per chunk, NaN for missing. A bare attribute is a
//     slice of its column, with no copy. Constant subtrees are folded.
//   - string: never materialized. A string expression is either a constant
//     or (column, lowered?), and every string predicate is decided per
//     dictionary entry once at compile time into a lookup table indexed by
//     code; per row it is one byte load. lower() maps the dictionary through
//     strings.ToLower once. Two string columns compare through a shared
//     numbering of their (possibly lowered) dictionary values.
//   - condition: 64 rows per uint64 word; and/or/not are word operations.

// chunkRows is the number of rows evaluated per step. It must be a multiple
// of 64.
const chunkRows = 4096

// Program is a rule condition compiled against one Table. It is immutable
// and safe for concurrent use.
type Program struct {
	t    *Table
	root boolNode
	nf   int // float64 scratch vectors needed per chunk
	nw   int // bitmap scratch vectors needed per chunk
}

// CompileVector compiles a condition that has passed rules.Check into a
// column program over t. The rule must have been checked against t's
// catalog.
func CompileVector(e rules.Expr, t *Table) (p *Program, err error) {
	defer func() {
		if r := recover(); r != nil {
			ce, ok := r.(compileErr)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("backtest: cannot compile %s: %s", rules.Print(e), string(ce))
		}
	}()
	c := &compiler{t: t}
	b := c.cond(e)
	return &Program{t: t, root: b.node(), nf: c.nf, nw: c.nw}, nil
}

type compileErr string

func fail(format string, args ...any) { panic(compileErr(fmt.Sprintf(format, args...))) }

// Eval evaluates the program over every row, in parallel across GOMAXPROCS.
func (p *Program) Eval() *Bitmap { return p.EvalWorkers(0) }

// EvalWorkers evaluates over every row with the given number of goroutines;
// 0 means GOMAXPROCS.
func (p *Program) EvalWorkers(workers int) *Bitmap {
	return EvalMany(p.t, []*Program{p}, 0, p.t.N, workers)[0]
}

// EvalRows evaluates rows [lo, hi) only; every other bit of the result is
// zero. With a table sorted by time this restricts a backtest to a window
// without touching the rest of the table.
func (p *Program) EvalRows(lo, hi, workers int) *Bitmap {
	return EvalMany(p.t, []*Program{p}, lo, hi, workers)[0]
}

// EvalMany evaluates several programs over rows [lo, hi) of t in one pass,
// chunk-major: each worker takes a chunk and runs every program over it
// before moving on, so a 50-rule set reads each column chunk from cache
// rather than streaming the table from memory 50 times. workers <= 0 means
// GOMAXPROCS.
func EvalMany(t *Table, progs []*Program, lo, hi, workers int) []*Bitmap {
	lo, hi = max(lo, 0), min(hi, t.N)
	out := make([]*Bitmap, len(progs))
	nf, nw := 0, 0
	for i, p := range progs {
		if p.t != t {
			panic("backtest: program compiled for a different table")
		}
		out[i] = NewBitmap(t.N)
		nf, nw = max(nf, p.nf), max(nw, p.nw)
	}
	if lo >= hi || len(progs) == 0 {
		return out
	}
	start := lo &^ 63 // chunks own whole words of the output
	nchunks := (hi - start + chunkRows - 1) / chunkRows
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, nchunks)

	var next atomic.Int64
	run := func() {
		s := newScratch(nf, nw)
		for {
			k := int(next.Add(1) - 1)
			if k >= nchunks {
				return
			}
			clo := start + k*chunkRows
			c := chunk{lo: clo, n: min(chunkRows, hi-clo), s: s}
			w0 := clo >> 6
			for i, p := range progs {
				p.root.eval(&c, out[i].W[w0:w0+words(c.n)])
			}
		}
	}
	if workers <= 1 {
		run()
	} else {
		var wg sync.WaitGroup
		for range workers {
			wg.Go(run)
		}
		wg.Wait()
	}
	if r := lo & 63; r != 0 {
		for _, b := range out {
			b.W[lo>>6] &^= 1<<uint(r) - 1
		}
	}
	return out
}

type scratch struct {
	f [][]float64
	w [][]uint64
}

func newScratch(nf, nw int) *scratch {
	s := &scratch{f: make([][]float64, nf), w: make([][]uint64, nw)}
	for i := range s.f {
		s.f[i] = make([]float64, chunkRows)
	}
	for i := range s.w {
		s.w[i] = make([]uint64, chunkRows/64)
	}
	return s
}

// chunk is rows [lo, lo+n) of the table.
type chunk struct {
	lo, n int
	s     *scratch
}

// ---- compilation -------------------------------------------------------

type compiler struct {
	t      *Table
	nf, nw int
}

func (c *compiler) fbuf() int { c.nf++; return c.nf - 1 }
func (c *compiler) wbuf() int { c.nw++; return c.nw - 1 }

// field resolves an attribute and checks that the table was built for the
// catalog the rule was checked against.
func (c *compiler) field(a *rules.Attr) schema.Field {
	f, ok := c.t.Catalog.Lookup(a.Name)
	if !ok || f.Kind != a.Field.Kind || f.Slot != a.Field.Slot {
		fail(":%s: is not resolved against this table's catalog (was the rule checked?)", a.Name)
	}
	return f
}

// numC is a compiled number expression: a constant or a vector node.
type numC struct {
	konst bool
	v     float64
	n     numNode
}

func (c *compiler) num(e rules.Expr) numC {
	switch e := e.(type) {
	case *rules.Attr:
		f := c.field(e)
		if f.Kind != schema.Number {
			fail(":%s: is not a number", e.Name)
		}
		return numC{n: colNum{c.t.Num[f.Slot]}}
	case *rules.NumberLit:
		return numC{konst: true, v: e.Value}
	case *rules.Unary:
		if e.Op != rules.OpNeg {
			fail("%s is not a number", rules.Print(e))
		}
		x := c.num(e.X)
		if x.konst {
			return numC{konst: true, v: -x.v}
		}
		return numC{n: negNum{x: x.n, buf: c.fbuf()}}
	case *rules.Binary:
		if !e.Op.IsArithmetic() {
			fail("%s is not a number", rules.Print(e))
		}
		x, y := c.num(e.X), c.num(e.Y)
		switch {
		case x.konst && y.konst:
			return numC{konst: true, v: arith(e.Op, x.v, y.v)}
		case y.konst:
			return numC{n: arithVC{op: e.Op, x: x.n, c: y.v, buf: c.fbuf()}}
		case x.konst:
			return numC{n: arithCV{op: e.Op, c: x.v, y: y.n, buf: c.fbuf()}}
		}
		return numC{n: arithVV{op: e.Op, x: x.n, y: y.n, buf: c.fbuf()}}
	}
	fail("%s is not a number", rules.Print(e))
	return numC{}
}

// arith is one arithmetic step with the rule language's semantics: IEEE
// arithmetic, except that dividing by zero (either sign) is missing.
func arith(op rules.Op, a, b float64) float64 {
	switch op {
	case rules.OpAdd:
		return a + b
	case rules.OpSub:
		return a - b
	case rules.OpMul:
		return a * b
	case rules.OpDiv:
		if b == 0 {
			return math.NaN()
		}
		return a / b
	}
	fail("unknown arithmetic operator %s", op)
	return 0
}

// compare is one comparison with the rule language's semantics: false when
// either side is missing, including for !=.
func compare(op rules.Op, a, b float64) bool {
	switch op {
	case rules.OpEq:
		return a == b
	case rules.OpNe:
		return a == a && b == b && a != b
	case rules.OpLt:
		return a < b
	case rules.OpLe:
		return a <= b
	case rules.OpGt:
		return a > b
	case rules.OpGe:
		return a >= b
	}
	fail("unknown comparison %s", op)
	return false
}

// strC is a compiled string expression. Strings are never materialized per
// row: an expression is a constant, or a column viewed through its
// dictionary, optionally lowered.
type strC struct {
	konst bool
	v     string // constant value, already lowered if it was
	col   int
	lower bool
}

func (c *compiler) str(e rules.Expr) strC {
	switch e := e.(type) {
	case *rules.Attr:
		f := c.field(e)
		if f.Kind != schema.String {
			fail(":%s: is not text", e.Name)
		}
		return strC{col: f.Slot}
	case *rules.StringLit:
		return strC{konst: true, v: e.Value}
	case *rules.Call:
		if e.Fn != rules.FuncLower || len(e.Args) != 1 {
			fail("%s is not text", rules.Print(e))
		}
		x := c.str(e.Args[0])
		if x.konst {
			x.v = strings.ToLower(x.v)
		} else {
			x.lower = true // lowering is idempotent, so lower(lower(x)) = lower(x)
		}
		return x
	}
	fail("%s is not text", rules.Print(e))
	return strC{}
}

// view returns the values a string column expression takes, indexed by
// dictionary code. Entry 0 is "" (missing) in both views, because
// strings.ToLower("") is "" and never maps anything else to "".
func (c *compiler) view(s strC) []string {
	if s.lower {
		return c.t.lowered(s.col)
	}
	return c.t.Dict[s.col]
}

// lut builds a per-code truth table for a predicate on a string column.
// Missing (code 0) is always false; pred sees only present values.
func (c *compiler) lut(s strC, pred func(v string) bool) boolNode {
	view := c.view(s)
	tbl := make([]uint8, len(view))
	for code, v := range view[1:] {
		if pred(v) {
			tbl[code+1] = 1
		}
	}
	return lutNode{codes: c.t.Str[s.col], lut: tbl}
}

// boolC is a compiled condition: a constant or a bitmap node.
type boolC struct {
	konst bool
	v     bool
	n     boolNode
}

func (b boolC) node() boolNode {
	if b.konst {
		return constBool(b.v)
	}
	return b.n
}

func konst(v bool) boolC { return boolC{konst: true, v: v} }

func (c *compiler) cond(e rules.Expr) boolC {
	switch e := e.(type) {
	case *rules.BoolLit:
		return konst(e.Value)
	case *rules.Unary:
		if e.Op != rules.OpNot {
			fail("%s is not a condition", rules.Print(e))
		}
		x := c.cond(e.X)
		if x.konst {
			return konst(!x.v)
		}
		return boolC{n: notNode{x.n}}
	case *rules.Binary:
		switch {
		case e.Op == rules.OpAnd || e.Op == rules.OpOr:
			return c.logical(e.Op, c.cond(e.X), c.cond(e.Y))
		case e.Op.IsComparison():
			switch e.X.Type() {
			case rules.TypeNumber:
				return c.compareNum(e.Op, c.num(e.X), c.num(e.Y))
			case rules.TypeString:
				return c.compareStr(e.Op, c.str(e.X), c.str(e.Y))
			}
		}
	case *rules.In:
		switch e.Kind {
		case schema.Number:
			return c.inNum(c.num(e.X), e.Numbers)
		case schema.String:
			return c.inStr(c.str(e.X), e.Strings)
		}
	case *rules.Call:
		switch {
		case e.Fn == rules.FuncIsMissing && len(e.Args) == 1:
			return c.isMissing(e.Args[0])
		case e.Fn == rules.FuncStartsWith && len(e.Args) == 2:
			return c.startsWith(c.str(e.Args[0]), c.str(e.Args[1]))
		}
	}
	fail("%s is not a condition", rules.Print(e))
	return boolC{}
}

// logical folds a constant operand away: with two-valued logic it either
// decides the result or drops out.
func (c *compiler) logical(op rules.Op, x, y boolC) boolC {
	decisive := op == rules.OpOr
	for _, pair := range [2][2]boolC{{x, y}, {y, x}} {
		if k, other := pair[0], pair[1]; k.konst {
			if k.v == decisive {
				return konst(decisive)
			}
			return other
		}
	}
	return boolC{n: logicNode{or: op == rules.OpOr, x: x.n, y: y.n, buf: c.wbuf()}}
}

// flip mirrors a comparison so the constant can go on the right.
func flip(op rules.Op) rules.Op {
	switch op {
	case rules.OpLt:
		return rules.OpGt
	case rules.OpLe:
		return rules.OpGe
	case rules.OpGt:
		return rules.OpLt
	case rules.OpGe:
		return rules.OpLe
	}
	return op
}

func (c *compiler) compareNum(op rules.Op, x, y numC) boolC {
	switch {
	case x.konst && y.konst:
		return konst(compare(op, x.v, y.v))
	case x.konst:
		x, y, op = y, x, flip(op)
	}
	if y.konst {
		if math.IsNaN(y.v) {
			return konst(false) // every comparison with missing is false
		}
		if !op.IsComparison() {
			fail("unknown comparison %s", op)
		}
		return boolC{n: cmpVC{op: op, x: x.n, c: y.v}}
	}
	if !op.IsComparison() {
		fail("unknown comparison %s", op)
	}
	return boolC{n: cmpVV{op: op, x: x.n, y: y.n}}
}

func (c *compiler) compareStr(op rules.Op, x, y strC) boolC {
	if op != rules.OpEq && op != rules.OpNe {
		fail("%s does not compare text", op)
	}
	ne := op == rules.OpNe
	if x.konst && !y.konst {
		x, y = y, x
	}
	switch {
	case x.konst: // both constant
		return konst(x.v != "" && y.v != "" && (x.v == y.v) != ne)
	case y.konst:
		if y.v == "" {
			return konst(false)
		}
		return boolC{n: c.lut(x, func(v string) bool { return (v == y.v) != ne })}
	}
	// Two columns: number the distinct values of both views together, so
	// that equal text gets equal ids whichever dictionary it came from, and
	// compare ids per row. Id 0 is missing on both sides.
	ids := map[string]uint32{"": 0}
	number := func(view []string) []uint32 {
		out := make([]uint32, len(view))
		for code, v := range view {
			id, ok := ids[v]
			if !ok {
				id = uint32(len(ids))
				ids[v] = id
			}
			out[code] = id
		}
		return out
	}
	return boolC{n: strPair{
		ne: ne,
		a:  c.t.Str[x.col], ida: number(c.view(x)),
		b: c.t.Str[y.col], idb: number(c.view(y)),
	}}
}

func (c *compiler) inNum(x numC, set []float64) boolC {
	if !slices.IsSorted(set) {
		fail("list values are not sorted (was the rule checked?)")
	}
	if x.konst {
		return konst(containsNum(set, x.v))
	}
	return boolC{n: inNumNode{x: x.n, set: set}}
}

// containsNum reports whether v is in the sorted set. NaN is in no set:
// binary search places it past the end, and == would reject it anyway. -0
// finds 0, as == requires.
func containsNum(set []float64, v float64) bool {
	i, _ := slices.BinarySearch(set, v)
	return i < len(set) && set[i] == v
}

func (c *compiler) inStr(x strC, set []string) boolC {
	has := func(v string) bool { _, ok := slices.BinarySearch(set, v); return ok }
	if !slices.IsSorted(set) {
		fail("list values are not sorted (was the rule checked?)")
	}
	if x.konst {
		return konst(x.v != "" && has(x.v))
	}
	return boolC{n: c.lut(x, has)}
}

func (c *compiler) isMissing(arg rules.Expr) boolC {
	switch arg.Type() {
	case rules.TypeNumber:
		x := c.num(arg)
		if x.konst {
			return konst(math.IsNaN(x.v))
		}
		return boolC{n: isNaNNode{x.n}}
	case rules.TypeString:
		x := c.str(arg)
		if x.konst {
			return konst(x.v == "")
		}
		tbl := make([]uint8, len(c.t.Dict[x.col]))
		tbl[0] = 1
		return boolC{n: lutNode{codes: c.t.Str[x.col], lut: tbl}}
	}
	fail("is_missing of %s", rules.Print(arg))
	return boolC{}
}

func (c *compiler) startsWith(x, p strC) boolC {
	switch {
	case x.konst && p.konst:
		return konst(x.v != "" && p.v != "" && strings.HasPrefix(x.v, p.v))
	case p.konst:
		if p.v == "" {
			return konst(false)
		}
		return boolC{n: c.lut(x, func(v string) bool { return strings.HasPrefix(v, p.v) })}
	case x.konst:
		if x.v == "" {
			return konst(false)
		}
		return boolC{n: c.lut(p, func(v string) bool { return strings.HasPrefix(x.v, v) })}
	}
	return boolC{n: prefixPair{a: c.t.Str[x.col], va: c.view(x), b: c.t.Str[p.col], vb: c.view(p)}}
}

// lowered returns strings.ToLower of every entry of dictionary col, computed
// once per table and column.
func (t *Table) lowered(col int) []string {
	t.lowerMu.Lock()
	defer t.lowerMu.Unlock()
	if t.lowerCache == nil {
		t.lowerCache = make(map[int][]string)
	}
	if v, ok := t.lowerCache[col]; ok {
		return v
	}
	d := t.Dict[col]
	v := make([]string, len(d))
	for i, s := range d {
		v[i] = strings.ToLower(s)
	}
	t.lowerCache[col] = v
	return v
}
