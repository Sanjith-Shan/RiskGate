package rules

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokEOL    tokenKind = iota // end of the rule's line
	tokIdent                   // bare word: a function name, or a mistake
	tokAttr                    // :name:
	tokList                    // @name
	tokNumber                  // 12, 3.5
	tokString                  // "text"
	tokLParen
	tokRParen
	tokLBrack
	tokRBrack
	tokComma
	tokOp // = != < <= > >= + - * /

	// Keywords.
	tokAllow
	tokBlock
	tokReview
	tokShadow
	tokIf
	tokAnd
	tokOr
	tokNot
	tokIn
	tokTrue
	tokFalse
)

var keywords = map[string]tokenKind{
	"allow": tokAllow, "block": tokBlock, "review": tokReview, "shadow": tokShadow,
	"if": tokIf, "and": tokAnd, "or": tokOr, "not": tokNot, "in": tokIn,
	"true": tokTrue, "false": tokFalse,
}

type token struct {
	kind tokenKind
	op   Op     // for tokOp
	text string // source text; for tokString the decoded value, for tokAttr/tokList the bare name
	num  float64
	sp   Span
}

// describe names a token for "expected X, found Y" messages.
func (t token) describe() string {
	switch t.kind {
	case tokEOL:
		return "the end of the line"
	case tokString:
		return strconv.Quote(t.text)
	case tokAttr:
		return ":" + t.text + ":"
	case tokList:
		return "@" + t.text
	}
	return t.text
}

// lexer turns one line of rule source into tokens. It never stops at an
// error: it records a diagnostic, makes the most plausible token (== becomes
// =, && becomes and), and carries on, so the parser can still report the
// next problem on the same line.
type lexer struct {
	src  string // the line, without its newline
	base Pos    // position of src[0] in the whole rule set
	i    int    // byte offset into src
	col  int    // rune column of src[i], 1-based
	toks []token
	errs Diagnostics
}

func lexLine(line string, base Pos) ([]token, Diagnostics) {
	lx := &lexer{src: line, base: base, col: base.Col}
	if !utf8.ValidString(line) {
		i := 0
		for i < len(line) {
			r, w := utf8.DecodeRuneInString(line[i:])
			if r == utf8.RuneError && w == 1 {
				break
			}
			i += w
		}
		p := lx.posAt(i, utf8.RuneCountInString(line[:i]))
		lx.errs = append(lx.errs, Diagnostic{
			Span: Span{p, p}, Line: strings.ToValidUTF8(line, "?"),
			Msg: "this line is not valid UTF-8 text.", Hint: "Re-save the rule file as UTF-8.",
		})
		return nil, lx.errs
	}
	lx.run()
	if len(lx.errs) > maxLexErrors {
		// Past a few, more errors on one line are noise: whatever is wrong
		// is wrong with the whole line.
		lx.errs = lx.errs[:maxLexErrors]
	}
	return lx.toks, lx.errs
}

const maxLexErrors = 3

func (lx *lexer) posAt(off, runes int) Pos {
	return Pos{Offset: lx.base.Offset + off, Line: lx.base.Line, Col: lx.base.Col + runes}
}

func (lx *lexer) pos() Pos {
	return Pos{Offset: lx.base.Offset + lx.i, Line: lx.base.Line, Col: lx.col}
}

func (lx *lexer) peek(k int) byte {
	if lx.i+k < len(lx.src) {
		return lx.src[lx.i+k]
	}
	return 0
}

// advance moves past n bytes that start at a rune boundary.
func (lx *lexer) advance(n int) {
	lx.col += utf8.RuneCountInString(lx.src[lx.i : lx.i+n])
	lx.i += n
}

func (lx *lexer) emit(kind tokenKind, start Pos, text string) *token {
	lx.toks = append(lx.toks, token{kind: kind, text: text, sp: Span{start, lx.pos()}})
	return &lx.toks[len(lx.toks)-1]
}

func (lx *lexer) errorf(sp Span, hint, format string, args ...any) {
	lx.errs = append(lx.errs, Diagnostic{Span: sp, Line: lx.src, Msg: fmt.Sprintf(format, args...), Hint: hint})
}

func isNameStart(c byte) bool {
	return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func isNameChar(c byte) bool { return isNameStart(c) || isDigit(c) }

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// nameEnd returns the offset just past the name starting at i. Names may
// contain any Unicode letter or digit: no catalog name does, but lexing
// :ämount: as a name lets the checker say "did you mean :amount:?" instead
// of complaining about a stray character.
func (lx *lexer) nameEnd(i int) int {
	for i < len(lx.src) {
		c := lx.src[i]
		if c < utf8.RuneSelf {
			if !isNameChar(c) {
				break
			}
			i++
			continue
		}
		r, w := utf8.DecodeRuneInString(lx.src[i:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		i += w
	}
	return i
}

// atNameStart reports whether a name starts at the current offset.
func (lx *lexer) atNameStart() bool {
	c := lx.src[lx.i]
	if c < utf8.RuneSelf {
		return isNameStart(c)
	}
	r, _ := utf8.DecodeRuneInString(lx.src[lx.i:])
	return unicode.IsLetter(r)
}

func (lx *lexer) run() {
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		start := lx.pos()
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			lx.advance(1)
		case c == '#':
			lx.advance(len(lx.src) - lx.i) // comment to end of line
		case lx.atNameStart():
			j := lx.nameEnd(lx.i)
			word := lx.src[lx.i:j]
			lx.advance(j - lx.i)
			kind, ok := keywords[word]
			if !ok {
				kind = tokIdent
			}
			lx.emit(kind, start, word)
		case isDigit(c):
			lx.number(start)
		case c == '.' && isDigit(lx.peek(1)):
			j := lx.i + 1
			for j < len(lx.src) && isDigit(lx.src[j]) {
				j++
			}
			text := lx.src[lx.i:j]
			lx.advance(j - lx.i)
			lx.errorf(Span{start, lx.pos()}, fmt.Sprintf("Write 0%s.", text), "a number must start with a digit.")
			n, _ := strconv.ParseFloat("0"+text, 64)
			lx.emit(tokNumber, start, text).num = n
		case c == '"':
			lx.str(start)
		case c == ':':
			lx.delimited(start, ':', tokAttr)
		case c == '@':
			lx.delimited(start, '@', tokList)
		default:
			lx.punct(start)
		}
	}
	lx.emit(tokEOL, lx.pos(), "")
}

// number lexes digits with an optional fraction. Exponents are rejected
// rather than supported: nobody writes 1e3 dollars in a fraud rule, and a
// typo like 10e0 is more likely than intent.
func (lx *lexer) number(start Pos) {
	j := lx.i
	for j < len(lx.src) && isDigit(lx.src[j]) {
		j++
	}
	if j < len(lx.src) && lx.src[j] == '.' {
		if j+1 < len(lx.src) && isDigit(lx.src[j+1]) {
			j++
			for j < len(lx.src) && isDigit(lx.src[j]) {
				j++
			}
		} else {
			text := lx.src[lx.i:j]
			lx.advance(j + 1 - lx.i)
			lx.errorf(Span{start, lx.pos()}, fmt.Sprintf("Write %s or %s.0.", text, text), "a number cannot end with a decimal point.")
			n, _ := strconv.ParseFloat(text, 64)
			lx.emit(tokNumber, start, text).num = n
			return
		}
	}
	text := lx.src[lx.i:j]
	if j < len(lx.src) && (isNameChar(lx.src[j]) || lx.src[j] == '.') {
		k := j
		for k < len(lx.src) && (isNameChar(lx.src[k]) || lx.src[k] == '.') {
			k++
		}
		bad := lx.src[lx.i:k]
		lx.advance(k - lx.i)
		hint := "Numbers are digits with an optional decimal part, like 250 or 0.75."
		if strings.ContainsAny(bad[len(text):], "eE") && !strings.ContainsFunc(bad[len(text)+1:], func(r rune) bool { return r < '0' || r > '9' }) {
			hint = "Write the number out in full; exponents like 1e3 are not supported."
		}
		lx.errorf(Span{start, lx.pos()}, hint, "%s is not a number.", bad)
		lx.emit(tokNumber, start, text)
		return
	}
	lx.advance(j - lx.i)
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		lx.errorf(Span{start, lx.pos()}, "", "%s is too large to be a number.", text)
		n = 0
	}
	lx.emit(tokNumber, start, text).num = n
}

// str lexes a double-quoted string. Escapes are \" \\ \n \t \r and \uXXXX.
func (lx *lexer) str(start Pos) {
	lx.advance(1)
	var b strings.Builder
	for {
		if lx.i >= len(lx.src) {
			lx.errorf(Span{start, lx.pos()}, `Add a closing " at the end of the text.`, "this text is missing its closing quote.")
			lx.emit(tokString, start, b.String())
			return
		}
		c := lx.src[lx.i]
		switch {
		case c == '"':
			lx.advance(1)
			lx.emit(tokString, start, b.String())
			return
		case c == '\\':
			escStart := lx.pos()
			var r rune = -1
			n := 2
			switch lx.peek(1) {
			case '"':
				r = '"'
			case '\\':
				r = '\\'
			case 'n':
				r = '\n'
			case 't':
				r = '\t'
			case 'r':
				r = '\r'
			case 'u':
				if lx.i+6 <= len(lx.src) {
					if v, err := strconv.ParseUint(lx.src[lx.i+2:lx.i+6], 16, 32); err == nil && utf8.ValidRune(rune(v)) {
						r, n = rune(v), 6
					}
				}
			}
			if r < 0 {
				// Consume the backslash and the following rune so the caret
				// covers exactly what was written.
				if lx.i+1 < len(lx.src) {
					_, w := utf8.DecodeRuneInString(lx.src[lx.i+1:])
					n = 1 + w
				} else {
					n = 1
				}
				lx.advance(n)
				lx.errorf(Span{escStart, lx.pos()}, `Write \\ for a backslash, \" for a quote, or \uXXXX for a character by its hex code.`, "unknown escape sequence in text.")
				continue
			}
			b.WriteRune(r)
			lx.advance(n)
		default:
			_, w := utf8.DecodeRuneInString(lx.src[lx.i:])
			b.WriteString(lx.src[lx.i : lx.i+w])
			lx.advance(w)
		}
	}
}

// delimited lexes :name: (delim ':') or @name (delim '@').
func (lx *lexer) delimited(start Pos, delim byte, kind tokenKind) {
	lx.advance(1)
	j := lx.nameEnd(lx.i)
	name := lx.src[lx.i:j]
	lx.advance(j - lx.i)
	if delim == '@' {
		if name == "" {
			lx.errorf(Span{start, lx.pos()}, "Write the list's name right after @, like @trusted_domains.", "@ must be followed by a list name.")
		}
		lx.emit(kind, start, name)
		return
	}
	if name == "" {
		lx.errorf(Span{start, lx.pos()}, "Attributes are written between colons, like :amount:.", "found a ':' with no attribute name after it.")
		lx.emit(kind, start, name)
		return
	}
	if lx.i < len(lx.src) && lx.src[lx.i] == ':' {
		lx.advance(1)
		lx.emit(kind, start, name)
		return
	}
	lx.errorf(Span{start, lx.pos()}, fmt.Sprintf("Add a colon after the name: :%s:.", name), "attribute :%s is missing its closing colon.", name)
	lx.emit(kind, start, name)
}

// punct lexes operators and brackets, and turns the operators people bring
// from other languages into the right token plus a pointed error.
func (lx *lexer) punct(start Pos) {
	two := ""
	if lx.i+2 <= len(lx.src) {
		two = lx.src[lx.i : lx.i+2]
	}
	simple := func(n int, kind tokenKind, op Op) {
		lx.advance(n)
		lx.emit(kind, start, lx.src[lx.i-n:lx.i]).op = op
	}
	// wrong reports an operator borrowed from another language and emits
	// the token it most likely meant, spelled canon.
	wrong := func(n int, kind tokenKind, op Op, canon, instead string) {
		lx.advance(n)
		text := lx.src[lx.i-n : lx.i]
		lx.errorf(Span{start, lx.pos()}, fmt.Sprintf("Write %s instead.", instead), "%s is not an operator in rules.", text)
		lx.emit(kind, start, canon).op = op
	}
	switch two {
	case "!=":
		simple(2, tokOp, OpNe)
		return
	case "<=":
		simple(2, tokOp, OpLe)
		return
	case ">=":
		simple(2, tokOp, OpGe)
		return
	case "==":
		wrong(2, tokOp, OpEq, "=", "a single =")
		return
	case "<>":
		wrong(2, tokOp, OpNe, "!=", "!=")
		return
	case "&&":
		wrong(2, tokAnd, OpInvalid, "and", "and")
		return
	case "||":
		wrong(2, tokOr, OpInvalid, "or", "or")
		return
	case "=<":
		wrong(2, tokOp, OpLe, "<=", "<=")
		return
	case "=>":
		wrong(2, tokOp, OpGe, ">=", ">=")
		return
	}
	switch c := lx.src[lx.i]; c {
	case '=':
		simple(1, tokOp, OpEq)
	case '<':
		simple(1, tokOp, OpLt)
	case '>':
		simple(1, tokOp, OpGt)
	case '+':
		simple(1, tokOp, OpAdd)
	case '-':
		simple(1, tokOp, OpSub)
	case '*':
		simple(1, tokOp, OpMul)
	case '/':
		simple(1, tokOp, OpDiv)
	case '(':
		simple(1, tokLParen, OpInvalid)
	case ')':
		simple(1, tokRParen, OpInvalid)
	case '[':
		simple(1, tokLBrack, OpInvalid)
	case ']':
		simple(1, tokRBrack, OpInvalid)
	case ',':
		simple(1, tokComma, OpInvalid)
	case '!':
		wrong(1, tokNot, OpInvalid, "not", "not")
	case '&':
		wrong(1, tokAnd, OpInvalid, "and", "and")
	case '|':
		wrong(1, tokOr, OpInvalid, "or", "or")
	case '\'':
		lx.singleQuoted(start)
	default:
		r, w := utf8.DecodeRuneInString(lx.src[lx.i:])
		lx.advance(w)
		sp := Span{start, lx.pos()}
		switch r {
		case '“', '”', '„', '‘', '’':
			lx.errorf(sp, `Use a plain double quote ("), not a curly one; editors often insert these when text is pasted.`, "curly quote %c found.", r)
			// Treat curly-quoted text as a string so the rest of the line
			// still checks: consume to the matching closing curly quote.
			lx.curlyString(start)
		default:
			// A run of junk (a pasted binary blob, say) is one mistake, so
			// it gets one diagnostic covering the run.
			if n := len(lx.errs); n > 0 && lx.errs[n-1].Span.End == start && strings.HasPrefix(lx.errs[n-1].Msg, "unexpected character") {
				lx.errs[n-1].Span.End = lx.pos()
				lx.errs[n-1].Msg = "unexpected characters."
				return
			}
			lx.errorf(sp, "", "unexpected character %q.", r)
		}
	}
}

// singleQuoted consumes 'text' and reports it, producing a string token so
// the rest of the rule can be checked.
func (lx *lexer) singleQuoted(start Pos) {
	lx.advance(1)
	j := strings.IndexByte(lx.src[lx.i:], '\'')
	if j < 0 {
		lx.errorf(Span{start, lx.pos()}, `Put text in double quotes, like "gmail.com".`, "text in rules uses double quotes, not single quotes.")
		return
	}
	text := lx.src[lx.i : lx.i+j]
	lx.advance(j + 1)
	lx.errorf(Span{start, lx.pos()}, fmt.Sprintf("Write %s.", strconv.Quote(text)), "text in rules uses double quotes, not single quotes.")
	lx.emit(tokString, start, text)
}

func (lx *lexer) curlyString(start Pos) {
	rest := lx.src[lx.i:]
	j := strings.IndexAny(rest, "“”„‘’\"")
	if j < 0 {
		return
	}
	text := rest[:j]
	_, w := utf8.DecodeRuneInString(rest[j:])
	lx.advance(j + w)
	lx.emit(tokString, start, text)
}
