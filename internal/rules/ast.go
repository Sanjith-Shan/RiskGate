package rules

import (
	"fmt"
	"math"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Pos is a position in rule source. Offset is a byte offset into the whole
// rule set; Line and Col are 1-based, and Col counts runes so that carets
// line up under non-ASCII text.
type Pos struct {
	Offset int
	Line   int
	Col    int
}

// Span is the half-open source range [Start, End) a node or token covers.
type Span struct {
	Start, End Pos
}

// Type is the static type of an expression.
type Type uint8

const (
	TypeInvalid Type = iota // not yet checked, or ill-typed
	TypeNumber
	TypeString
	TypeBool
)

func (t Type) String() string {
	switch t {
	case TypeNumber:
		return "number"
	case TypeString:
		return "string"
	case TypeBool:
		return "true/false condition"
	}
	return "invalid"
}

func typeOfKind(k schema.Kind) Type {
	switch k {
	case schema.Number:
		return TypeNumber
	case schema.String:
		return TypeString
	}
	return TypeInvalid
}

// Op is a unary or binary operator.
type Op uint8

const (
	OpInvalid Op = iota
	OpOr
	OpAnd
	OpNot
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpNeg
)

var opText = [...]string{
	OpInvalid: "?", OpOr: "or", OpAnd: "and", OpNot: "not",
	OpEq: "=", OpNe: "!=", OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">=",
	OpAdd: "+", OpSub: "-", OpMul: "*", OpDiv: "/", OpNeg: "-",
}

func (o Op) String() string {
	if int(o) < len(opText) {
		return opText[o]
	}
	return fmt.Sprintf("Op(%d)", uint8(o))
}

// IsComparison reports whether o is one of = != < <= > >=.
func (o Op) IsComparison() bool { return o >= OpEq && o <= OpGe }

// IsArithmetic reports whether o is a binary + - * /.
func (o Op) IsArithmetic() bool { return o >= OpAdd && o <= OpDiv }

// Binding powers, lowest first. The parser and the printer share this table,
// which is what keeps print(parse(s)) and parse(print(ast)) consistent.
const (
	precLowest = iota
	precOr
	precAnd
	precNot
	precCompare // = != < <= > >= in
	precSum     // + -
	precProduct // * /
	precUnary   // prefix -
	precAtom    // literals, attributes, calls: never parenthesized
)

func (o Op) prec() int {
	switch o {
	case OpOr:
		return precOr
	case OpAnd:
		return precAnd
	case OpNot:
		return precNot
	case OpAdd, OpSub:
		return precSum
	case OpMul, OpDiv:
		return precProduct
	case OpNeg:
		return precUnary
	}
	if o.IsComparison() {
		return precCompare
	}
	return precLowest
}

// Func identifies a built-in function.
type Func uint8

const (
	FuncInvalid    Func = iota
	FuncIsMissing       // is_missing(number or string) -> bool
	FuncLower           // lower(string) -> string, as strings.ToLower
	FuncStartsWith      // starts_with(string, string) -> bool
)

type builtin struct {
	fn      Func
	name    string
	params  string // for arity messages
	example string
}

var builtins = []builtin{
	{FuncIsMissing, "is_missing", "an attribute", `is_missing(:device_info:)`},
	{FuncLower, "lower", "some text", `lower(:purchaser_email_domain:) = "gmail.com"`},
	{FuncStartsWith, "starts_with", "some text and a prefix", `starts_with(:device_info:, "SM-")`},
}

func lookupFunc(name string) (builtin, bool) {
	for _, b := range builtins {
		if b.name == name {
			return b, true
		}
	}
	return builtin{}, false
}

func (f Func) String() string {
	for _, b := range builtins {
		if b.fn == f {
			return b.name
		}
	}
	return fmt.Sprintf("Func(%d)", uint8(f))
}

func (f Func) arity() int {
	if f == FuncStartsWith {
		return 2
	}
	return 1
}

// Expr is an expression node. The concrete types are *Attr, *NumberLit,
// *StringLit, *BoolLit, *Unary, *Binary, *In and *Call; a type switch over
// them is exhaustive. *ListRef and *ListLit appear only as In.List.
//
// Nodes come out of the parser unresolved. Check fills in the fields marked
// "set by Check" and, for a well-typed tree, Type then reports the node's
// static type. Everything another evaluator needs (slots, kinds, list values,
// builtins) is on the nodes, so no side table is required.
type Expr interface {
	Span() Span
	Type() Type
	exprNode()
}

// Attr is an attribute reference, :name:.
type Attr struct {
	Name  string
	Field schema.Field // set by Check: Kind and Slot index into schema.Row
	Sp    Span
}

// NumberLit is a non-negative numeric literal. A negative constant is a
// Unary{OpNeg} around one, exactly as it is written.
type NumberLit struct {
	Value float64
	Sp    Span
}

// StringLit is a double-quoted string literal. It is never empty: the
// checker rejects "" because the empty string means missing.
type StringLit struct {
	Value string
	Sp    Span
}

// BoolLit is true or false.
type BoolLit struct {
	Value bool
	Sp    Span
}

// Unary is not X or -X.
type Unary struct {
	Op Op // OpNot or OpNeg
	X  Expr
	Sp Span
}

// Binary is X op Y for logical, comparison and arithmetic operators.
type Binary struct {
	Op   Op
	X, Y Expr
	Sp   Span
}

// In is X in List. X is a number or a string, and List is a *ListRef or a
// *ListLit of the same kind.
type In struct {
	X    Expr
	List Expr
	// Set by Check: the list's element kind and its values, deduplicated and
	// sorted, whether they came from a named list or a literal one. Exactly
	// one of Strings and Numbers is used, according to Kind.
	Kind    schema.Kind
	Strings []string
	Numbers []float64
	Sp      Span
}

// Call is a built-in function call.
type Call struct {
	Name string
	Args []Expr
	Fn   Func // set by Check
	Sp   Span
}

// ListRef is a named list, @name, resolved against Env.Lists.
type ListRef struct {
	Name string
	Sp   Span
}

// ListLit is a literal list, ["a", "b"] or [1, -2.5]. Items are *StringLit,
// *NumberLit, or Unary{OpNeg} around a *NumberLit.
type ListLit struct {
	Items []Expr
	Sp    Span
}

func (e *Attr) Span() Span      { return e.Sp }
func (e *NumberLit) Span() Span { return e.Sp }
func (e *StringLit) Span() Span { return e.Sp }
func (e *BoolLit) Span() Span   { return e.Sp }
func (e *Unary) Span() Span     { return e.Sp }
func (e *Binary) Span() Span    { return e.Sp }
func (e *In) Span() Span        { return e.Sp }
func (e *Call) Span() Span      { return e.Sp }
func (e *ListRef) Span() Span   { return e.Sp }
func (e *ListLit) Span() Span   { return e.Sp }

func (e *Attr) Type() Type      { return typeOfKind(e.Field.Kind) }
func (e *NumberLit) Type() Type { return TypeNumber }
func (e *StringLit) Type() Type { return TypeString }
func (e *BoolLit) Type() Type   { return TypeBool }
func (e *In) Type() Type        { return TypeBool }
func (e *ListRef) Type() Type   { return TypeInvalid }
func (e *ListLit) Type() Type   { return TypeInvalid }

func (e *Unary) Type() Type {
	if e.Op == OpNeg {
		return TypeNumber
	}
	return TypeBool
}

func (e *Binary) Type() Type {
	if e.Op.IsArithmetic() {
		return TypeNumber
	}
	return TypeBool
}

func (e *Call) Type() Type {
	switch b, _ := lookupFunc(e.Name); b.fn {
	case FuncLower:
		return TypeString
	case FuncIsMissing, FuncStartsWith:
		return TypeBool
	}
	return TypeInvalid
}

func (*Attr) exprNode()      {}
func (*NumberLit) exprNode() {}
func (*StringLit) exprNode() {}
func (*BoolLit) exprNode()   {}
func (*Unary) exprNode()     {}
func (*Binary) exprNode()    {}
func (*In) exprNode()        {}
func (*Call) exprNode()      {}
func (*ListRef) exprNode()   {}
func (*ListLit) exprNode()   {}

// Action is what a matching rule does to a payment.
type Action uint8

const (
	Allow Action = iota + 1
	Block
	Review
)

func (a Action) String() string {
	switch a {
	case Allow:
		return "allow"
	case Block:
		return "block"
	case Review:
		return "review"
	}
	return fmt.Sprintf("Action(%d)", uint8(a))
}

// Rule is one line of a rule set: [shadow] <action> if <condition>.
type Rule struct {
	Shadow bool
	Action Action
	Cond   Expr
	Index  int    // position among the rule set's rules, from 0
	Text   string // the rule as written, without surrounding space or comment
	Sp     Span

	src string // the whole source line, so diagnostics can quote it
}

// Span returns the source range of the whole rule.
func (r *Rule) Span() Span { return r.Sp }

// Inspect walks e depth-first, calling f on each node before its children
// (including the *ListRef or *ListLit of an In). If f returns false the
// node's children are skipped.
func Inspect(e Expr, f func(Expr) bool) {
	if e == nil || !f(e) {
		return
	}
	switch e := e.(type) {
	case *Unary:
		Inspect(e.X, f)
	case *Binary:
		Inspect(e.X, f)
		Inspect(e.Y, f)
	case *In:
		Inspect(e.X, f)
		Inspect(e.List, f)
	case *Call:
		for _, a := range e.Args {
			Inspect(a, f)
		}
	case *ListLit:
		for _, it := range e.Items {
			Inspect(it, f)
		}
	}
}

// Equal reports whether a and b are the same tree. Spans and everything set
// by Check are ignored, so a parsed tree equals a generated one. Number
// literals compare by bit pattern, which is stricter than == and exactly what
// a print/parse round trip must preserve.
func Equal(a, b Expr) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case *Attr:
		b, ok := b.(*Attr)
		return ok && a.Name == b.Name
	case *NumberLit:
		b, ok := b.(*NumberLit)
		return ok && math.Float64bits(a.Value) == math.Float64bits(b.Value)
	case *StringLit:
		b, ok := b.(*StringLit)
		return ok && a.Value == b.Value
	case *BoolLit:
		b, ok := b.(*BoolLit)
		return ok && a.Value == b.Value
	case *Unary:
		b, ok := b.(*Unary)
		return ok && a.Op == b.Op && Equal(a.X, b.X)
	case *Binary:
		b, ok := b.(*Binary)
		return ok && a.Op == b.Op && Equal(a.X, b.X) && Equal(a.Y, b.Y)
	case *In:
		b, ok := b.(*In)
		return ok && Equal(a.X, b.X) && Equal(a.List, b.List)
	case *Call:
		b, ok := b.(*Call)
		return ok && a.Name == b.Name && equalList(a.Args, b.Args)
	case *ListRef:
		b, ok := b.(*ListRef)
		return ok && a.Name == b.Name
	case *ListLit:
		b, ok := b.(*ListLit)
		return ok && equalList(a.Items, b.Items)
	}
	return false
}

func equalList(a, b []Expr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// EqualRule reports whether two rules have the same shadow flag, action and
// condition, ignoring spans, text and index.
func EqualRule(a, b *Rule) bool {
	return a.Shadow == b.Shadow && a.Action == b.Action && Equal(a.Cond, b.Cond)
}
