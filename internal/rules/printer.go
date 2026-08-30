package rules

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Print returns the canonical text of e: single spaces around binary
// operators, double-quoted strings, the shortest decimal form of each
// number, and only the parentheses the precedence rules require.
// Parse(Print(e)) yields a tree Equal to e for every tree the parser can
// produce and every tree Generate builds.
func Print(e Expr) string {
	var b strings.Builder
	printExpr(&b, e)
	return b.String()
}

// String returns the canonical text of the rule, including a leading
// "shadow " for shadow rules.
func (r *Rule) String() string {
	var b strings.Builder
	if r.Shadow {
		b.WriteString("shadow ")
	}
	b.WriteString(r.Action.String())
	b.WriteString(" if ")
	printExpr(&b, r.Cond)
	return b.String()
}

// exprPrec is the binding power of e's outermost operator.
func exprPrec(e Expr) int {
	switch e := e.(type) {
	case *Binary:
		return e.Op.prec()
	case *Unary:
		return e.Op.prec()
	case *In:
		return precCompare
	}
	return precAtom
}

// printOperand prints e, parenthesized if it binds looser than the context
// allows. min is the lowest precedence e may have without parentheses.
func printOperand(b *strings.Builder, e Expr, min int) {
	if exprPrec(e) < min {
		b.WriteByte('(')
		printExpr(b, e)
		b.WriteByte(')')
		return
	}
	printExpr(b, e)
}

func printExpr(b *strings.Builder, e Expr) {
	switch e := e.(type) {
	case *Attr:
		b.WriteByte(':')
		b.WriteString(e.Name)
		b.WriteByte(':')
	case *NumberLit:
		b.WriteString(formatNumber(e.Value))
	case *StringLit:
		b.WriteString(quote(e.Value))
	case *BoolLit:
		b.WriteString(strconv.FormatBool(e.Value))
	case *Unary:
		if e.Op == OpNot {
			b.WriteString("not ")
			// The operand of not may be anything from not upward:
			// comparisons, arithmetic, atoms, another not.
			printOperand(b, e.X, precNot)
			return
		}
		b.WriteByte('-')
		printOperand(b, e.X, precUnary)
	case *Binary:
		p := e.Op.prec()
		// Left-associative: a left operand at the same level needs no
		// parentheses, a right one does ((a - b) - c prints bare, a - (b - c)
		// does not). Comparisons do not chain, so a comparison operand of a
		// comparison is always parenthesized.
		left := p
		if p == precCompare {
			left = p + 1
		}
		printOperand(b, e.X, left)
		b.WriteByte(' ')
		b.WriteString(e.Op.String())
		b.WriteByte(' ')
		printOperand(b, e.Y, p+1)
	case *In:
		printOperand(b, e.X, precCompare+1)
		b.WriteString(" in ")
		printExpr(b, e.List)
	case *Call:
		b.WriteString(e.Name)
		b.WriteByte('(')
		for i, a := range e.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			printExpr(b, a)
		}
		b.WriteByte(')')
	case *ListRef:
		b.WriteByte('@')
		b.WriteString(e.Name)
	case *ListLit:
		b.WriteByte('[')
		for i, it := range e.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			printExpr(b, it)
		}
		b.WriteByte(']')
	default:
		b.WriteString("<?>")
	}
}

// formatNumber prints the shortest decimal that parses back to exactly v,
// without an exponent, since the lexer does not accept exponents.
func formatNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// quote writes s as a rule string literal. It escapes only what the lexer
// requires, plus control characters, so non-ASCII text stays readable.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				b.WriteString(`\u`)
				h := strconv.FormatInt(int64(r), 16)
				b.WriteString(strings.Repeat("0", 4-len(h)))
				b.WriteString(h)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
