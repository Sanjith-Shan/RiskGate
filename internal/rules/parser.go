package rules

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse parses a rule set: one rule per line, where a rule is
//
//	[shadow] allow|block|review if <condition>
//
// Blank lines and # comments (whole-line or trailing) are ignored. Parse
// keeps going after a bad line so that every syntax error in the set is
// reported at once; the returned error, if any, is a Diagnostics. The rules
// that did parse are returned either way, unresolved: run Check before
// compiling them.
//
// Operator precedence, loosest first:
//
//	or
//	and
//	not
//	= != < <= > >= in
//	+ -
//	* /
//	unary -
//
// So `a or b and c` parses as `a or (b and c)`, just as `1 + 2 * 3` is
// `1 + (2 * 3)`: and binds tighter than or. `not` sits between and and the
// comparisons, so `not :amount: > 100 and x` is `(not (:amount: > 100)) and x`.
// Binary operators are left-associative; comparisons do not chain.
func Parse(src string) ([]*Rule, error) {
	rules, ds := parseSource(src, nil)
	return rules, ds.Err()
}

// ParseRule parses a single rule. src must hold exactly one rule.
func ParseRule(src string) (*Rule, error) {
	rules, ds := parseSource(src, nil)
	if err := ds.Err(); err != nil {
		return nil, err
	}
	if len(rules) != 1 {
		return nil, fmt.Errorf("rules: expected exactly one rule, found %d", len(rules))
	}
	return rules[0], nil
}

// ParseExpr parses a single-line condition on its own, without an action.
func ParseExpr(src string) (Expr, error) {
	if strings.ContainsRune(src, '\n') {
		return nil, fmt.Errorf("rules: a condition must fit on one line")
	}
	toks, ds := lexLine(src, Pos{Line: 1, Col: 1})
	if ds.HasErrors() {
		return nil, ds
	}
	p := &parser{toks: toks, line: src}
	var e Expr
	func() {
		defer p.recoverBail()
		e = p.parseExpr(precLowest)
		p.expectEnd()
	}()
	if p.errs.HasErrors() {
		return nil, p.errs
	}
	return e, nil
}

// parseSource parses every line of src. env, when non-nil, lets error
// messages use the catalog: "amount" without colons can then be recognized
// as the attribute :amount:.
func parseSource(src string, env *Env) ([]*Rule, Diagnostics) {
	var (
		rules []*Rule
		ds    Diagnostics
		off   int
		index int
	)
	for lineNo := 1; off <= len(src); lineNo++ {
		end := strings.IndexByte(src[off:], '\n')
		if end < 0 {
			end = len(src) - off
		}
		line := strings.TrimSuffix(src[off:off+end], "\r")
		base := Pos{Offset: off, Line: lineNo, Col: 1}
		off += end + 1

		toks, lexErrs := lexLine(line, base)
		if len(lexErrs) > 0 {
			ds = append(ds, lexErrs...)
			index++
			continue
		}
		if len(toks) == 1 { // blank or comment-only
			continue
		}
		p := &parser{toks: toks, line: line, env: env}
		if r := p.parseRule(); r != nil {
			r.Index = index
			r.Text = line[toks[0].sp.Start.Offset-base.Offset : toks[len(toks)-2].sp.End.Offset-base.Offset]
			r.src = line
			rules = append(rules, r)
		}
		ds = append(ds, p.errs...)
		index++
	}
	return rules, ds
}

// parser is a Pratt parser over one line's tokens. Each rule is parsed until
// its first syntax error, because after a syntax error the parser can only
// guess what the author meant, and a guess produces confusing follow-on
// errors. Other lines are unaffected.
type parser struct {
	toks []token
	p    int
	line string
	env  *Env
	errs Diagnostics
}

// bailout unwinds the parser to the rule boundary after an error. Using
// panic here keeps every parse function free of error plumbing; it never
// escapes the package.
type bailout struct{}

func (p *parser) recoverBail() {
	if r := recover(); r != nil {
		if _, ok := r.(bailout); !ok {
			panic(r)
		}
	}
}

func (p *parser) peek() token { return p.toks[p.p] }

func (p *parser) peekAt(k int) token {
	if p.p+k < len(p.toks) {
		return p.toks[p.p+k]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) next() token {
	t := p.toks[p.p]
	if t.kind != tokEOL {
		p.p++
	}
	return t
}

func (p *parser) fail(sp Span, hint, format string, args ...any) {
	p.errs = append(p.errs, Diagnostic{Span: sp, Line: p.line, Msg: fmt.Sprintf(format, args...), Hint: hint})
	panic(bailout{})
}

func (p *parser) parseRule() (r *Rule) {
	defer p.recoverBail()
	first := p.peek()
	shadow := false
	if first.kind == tokShadow {
		shadow = true
		p.next()
	}
	at := p.next()
	var action Action
	switch at.kind {
	case tokAllow:
		action = Allow
	case tokBlock:
		action = Block
	case tokReview:
		action = Review
	case tokIdent:
		hint := "A rule starts with allow, block or review."
		if s := suggestAction(at.text); s != "" {
			hint = fmt.Sprintf("Did you mean %s?", s)
		}
		p.fail(at.sp, hint, "unknown action %s.", at.text)
	case tokIf:
		p.fail(at.sp, "Start the rule with allow, block or review, like: block if ...", "this rule has no action.")
	default:
		what := "a rule must start with allow, block or review"
		if shadow {
			what = "shadow must be followed by allow, block or review"
		}
		p.fail(at.sp, `For example: block if :amount: > 1000`, "%s, but found %s.", what, at.describe())
	}
	if t := p.peek(); t.kind != tokIf {
		if t.kind == tokEOL {
			p.fail(t.sp, fmt.Sprintf("Write: %s if <condition>.", action), "expected if after %s.", action)
		}
		if t.kind == tokIdent && (t.text == "when" || t.text == "where" || strings.EqualFold(t.text, "if")) {
			p.fail(t.sp, fmt.Sprintf("Write: %s if <condition>.", action), "expected if after %s, but found %s.", action, t.text)
		}
		p.fail(Span{t.sp.Start, t.sp.Start}, fmt.Sprintf("Add if after %s: %s if <condition>.", action, action),
			"expected if after %s, but found %s.", action, t.describe())
	}
	p.next()
	if t := p.peek(); t.kind == tokEOL {
		p.fail(t.sp, fmt.Sprintf("For example: %s if :amount: > 1000", action), "expected a condition after if.")
	}
	cond := p.parseExpr(precLowest)
	p.expectEnd()
	return &Rule{
		Shadow: shadow,
		Action: action,
		Cond:   cond,
		Sp:     Span{first.sp.Start, p.toks[p.p-1].sp.End},
	}
}

var actionSynonyms = map[string]string{
	"deny": "block", "reject": "block", "decline": "block", "refuse": "block", "stop": "block", "ban": "block",
	"accept": "allow", "approve": "allow", "permit": "allow", "pass": "allow", "whitelist": "allow", "allowlist": "allow",
	"flag": "review", "hold": "review", "check": "review", "inspect": "review", "manual_review": "review",
}

func suggestAction(word string) string {
	if s, ok := actionSynonyms[strings.ToLower(word)]; ok {
		return s
	}
	return suggest(word, []string{"allow", "block", "review"})
}

// expectEnd reports anything left on the line after a complete condition.
func (p *parser) expectEnd() {
	t := p.peek()
	switch t.kind {
	case tokEOL:
		return
	case tokRParen:
		p.fail(t.sp, "Remove it, or add a matching ( earlier.", "this ) has no matching (.")
	case tokNot:
		p.fail(t.sp, "Did you mean and not?", "unexpected not after a complete condition.")
	case tokIdent:
		if kw, ok := keywords[strings.ToLower(t.text)]; ok && (kw == tokAnd || kw == tokOr || kw == tokIn) {
			p.fail(t.sp, fmt.Sprintf("Keywords are lowercase: write %s.", strings.ToLower(t.text)), "unexpected %s.", t.text)
		}
	case tokAllow, tokBlock, tokReview, tokShadow:
		p.fail(t.sp, "Put each rule on its own line.", "unexpected %s in the middle of a rule.", t.text)
	}
	p.fail(t.sp, "Join conditions with and or or.", "expected and, or, or the end of the rule, but found %s.", t.describe())
}

// parseExpr is the Pratt loop: parse a prefix expression, then keep folding
// infix operators into it while they bind tighter than minPrec.
func (p *parser) parseExpr(minPrec int) Expr {
	left := p.parsePrefix()
	prevCompare := false
	for {
		t := p.peek()
		var op Op
		isIn, notIn := false, false
		switch t.kind {
		case tokOr:
			op = OpOr
		case tokAnd:
			op = OpAnd
		case tokOp:
			op = t.op
		case tokIn:
			isIn = true
		case tokNot:
			// "x not in @list" is the one infix use of not.
			if p.peekAt(1).kind != tokIn {
				return left
			}
			isIn, notIn = true, true
		default:
			return left
		}
		prec := op.prec()
		if isIn {
			prec = precCompare
		}
		if prec <= minPrec {
			return left
		}
		if prec == precCompare && prevCompare {
			p.fail(t.sp, "Join separate comparisons with and, like :a: > 1 and :a: < 5.", "comparisons cannot be chained.")
		}
		prevCompare = prec == precCompare
		p.next()
		if isIn {
			if notIn {
				p.next()
			}
			list := p.parseList()
			in := &In{X: left, List: list, Sp: Span{left.Span().Start, list.Span().End}}
			left = in
			if notIn {
				left = &Unary{Op: OpNot, X: in, Sp: in.Sp}
			}
			continue
		}
		if p.peek().kind == tokEOL {
			p.fail(p.peek().sp, fmt.Sprintf("Add a value after %s.", t.text), "the rule ends right after %s.", t.text)
		}
		right := p.parseExpr(prec)
		left = &Binary{Op: op, X: left, Y: right, Sp: Span{left.Span().Start, right.Span().End}}
	}
}

func (p *parser) parsePrefix() Expr {
	t := p.next()
	switch t.kind {
	case tokNumber:
		return &NumberLit{Value: t.num, Sp: t.sp}
	case tokString:
		return &StringLit{Value: t.text, Sp: t.sp}
	case tokTrue, tokFalse:
		return &BoolLit{Value: t.kind == tokTrue, Sp: t.sp}
	case tokAttr:
		return &Attr{Name: t.text, Sp: t.sp}
	case tokNot:
		x := p.parseOperand(t, precNot)
		return &Unary{Op: OpNot, X: x, Sp: Span{t.sp.Start, x.Span().End}}
	case tokOp:
		switch t.op {
		case OpSub:
			x := p.parseOperand(t, precUnary)
			return &Unary{Op: OpNeg, X: x, Sp: Span{t.sp.Start, x.Span().End}}
		case OpAdd:
			p.fail(t.sp, "Remove the +; numbers are positive unless they start with -.", "unexpected + before a value.")
		}
		p.fail(t.sp, fmt.Sprintf("Put a value on each side of %s, like :amount: %s 100.", t.text, t.text), "expected a value before %s.", t.text)
	case tokLParen:
		inner := p.parseExpr(precLowest)
		if c := p.peek(); c.kind != tokRParen {
			if c.kind == tokEOL {
				p.fail(t.sp, "Add a ) where the group ends.", "this ( is never closed.")
			}
			p.fail(c.sp, fmt.Sprintf("Close the ( from column %d with ), or join conditions with and or or.", t.sp.Start.Col),
				"expected ), but found %s.", c.describe())
		}
		p.next()
		return inner
	case tokIdent:
		if p.peek().kind == tokLParen {
			return p.parseCall(t)
		}
		p.bareWord(t)
	case tokList:
		p.fail(t.sp, fmt.Sprintf("Write :attribute: in @%s.", t.text), "a list can only come after in.")
	case tokLBrack:
		p.fail(t.sp, `Write :attribute: in ["a", "b"].`, "a [...] list can only come after in.")
	case tokEOL:
		p.fail(t.sp, "", "expected a value, but the rule ends here.")
	case tokRParen:
		p.fail(t.sp, "Put a value or condition inside the parentheses.", "expected a value before ).")
	}
	p.fail(t.sp, "", "expected a value, but found %s.", t.describe())
	return nil
}

// parseOperand parses the operand of a prefix operator, with a message that
// names the operator when the operand is missing.
func (p *parser) parseOperand(op token, prec int) Expr {
	if p.peek().kind == tokEOL {
		p.fail(p.peek().sp, fmt.Sprintf("Add a value after %s.", op.text), "the rule ends right after %s.", op.text)
	}
	return p.parseExpr(prec)
}

// bareWord reports a word that is neither a keyword nor a function call. It
// is nearly always an attribute without its colons or text without quotes.
func (p *parser) bareWord(t token) {
	w := t.text
	if _, ok := keywords[strings.ToLower(w)]; ok && strings.ToLower(w) != w {
		p.fail(t.sp, fmt.Sprintf("Keywords are lowercase: write %s.", strings.ToLower(w)), "unexpected %s.", w)
	}
	attrHint := fmt.Sprintf("Attributes are written between colons: :%s:.", w)
	textHint := fmt.Sprintf("Text is written in double quotes: %s.", strconv.Quote(w))
	if p.env != nil && p.env.Catalog != nil {
		if _, ok := p.env.Catalog.Lookup(w); ok {
			p.fail(t.sp, attrHint, "%s needs colons around it.", w)
		}
		if s := suggest(w, p.env.Catalog.Names()); s != "" {
			p.fail(t.sp, fmt.Sprintf("Attributes are written between colons; did you mean :%s:?", s), "unexpected word %s.", w)
		}
	}
	// After a comparison or inside a list, a bare word is most likely a value.
	if p.p >= 2 {
		if prev := p.toks[p.p-2]; prev.kind == tokOp && prev.op.IsComparison() || prev.kind == tokLBrack || prev.kind == tokComma {
			p.fail(t.sp, textHint, "unexpected word %s.", w)
		}
	}
	p.fail(t.sp, fmt.Sprintf("If %s is an attribute, write :%s:. If it is text, write %s.", w, w, strconv.Quote(w)), "unexpected word %s.", w)
}

func (p *parser) parseCall(name token) Expr {
	p.next() // (
	var args []Expr
	if p.peek().kind != tokRParen {
		for {
			if p.peek().kind == tokEOL {
				p.fail(name.sp, fmt.Sprintf("Add a ) after the arguments to %s.", name.text), "the call to %s is never closed.", name.text)
			}
			args = append(args, p.parseExpr(precLowest))
			if p.peek().kind != tokComma {
				break
			}
			p.next()
		}
	}
	c := p.peek()
	if c.kind != tokRParen {
		if c.kind == tokEOL {
			p.fail(name.sp, fmt.Sprintf("Add a ) after the arguments to %s.", name.text), "the call to %s is never closed.", name.text)
		}
		p.fail(c.sp, "Separate arguments with commas and close the call with ).", "expected , or ) in the call to %s, but found %s.", name.text, c.describe())
	}
	p.next()
	return &Call{Name: name.text, Args: args, Sp: Span{name.sp.Start, c.sp.End}}
}

func (p *parser) parseList() Expr {
	t := p.next()
	switch t.kind {
	case tokList:
		return &ListRef{Name: t.text, Sp: t.sp}
	case tokLBrack:
	case tokLParen:
		p.fail(t.sp, `Lists use square brackets: ["a", "b"].`, "expected a list after in, but found (.")
	case tokEOL:
		p.fail(t.sp, `Name a list, like @trusted_domains, or write one out, like ["a", "b"].`, "expected a list after in.")
	default:
		p.fail(t.sp, `Name a list, like @trusted_domains, or write one out, like ["a", "b"].`, "expected a list after in, but found %s.", t.describe())
	}
	if c := p.peek(); c.kind == tokRBrack {
		p.fail(Span{t.sp.Start, c.sp.End}, `Add values, like ["a", "b"].`, "this list is empty, so nothing would ever be in it.")
	}
	var items []Expr
	for {
		items = append(items, p.parseListItem(t))
		c := p.next()
		switch c.kind {
		case tokComma:
			continue
		case tokRBrack:
			return &ListLit{Items: items, Sp: Span{t.sp.Start, c.sp.End}}
		case tokEOL:
			p.fail(t.sp, "Add a ] after the last item.", "this [ is never closed.")
		}
		p.fail(c.sp, "Separate list items with commas.", "expected , or ] in the list, but found %s.", c.describe())
	}
}

func (p *parser) parseListItem(open token) Expr {
	t := p.next()
	switch t.kind {
	case tokString:
		return &StringLit{Value: t.text, Sp: t.sp}
	case tokNumber:
		return &NumberLit{Value: t.num, Sp: t.sp}
	case tokOp:
		if n := p.peek(); t.op == OpSub && n.kind == tokNumber {
			p.next()
			return &Unary{Op: OpNeg, X: &NumberLit{Value: n.num, Sp: n.sp}, Sp: Span{t.sp.Start, n.sp.End}}
		}
	case tokEOL:
		p.fail(open.sp, "Add a ] after the last item.", "this [ is never closed.")
	case tokIdent:
		p.fail(t.sp, fmt.Sprintf("Text is written in double quotes: %s.", strconv.Quote(t.text)), "unexpected word %s in the list.", t.text)
	case tokRBrack:
		p.fail(t.sp, "Remove the extra comma.", "expected a list item before ].")
	}
	p.fail(t.sp, `A list holds text in quotes or plain numbers, like ["a", "b"] or [1, 2].`, "%s cannot be a list item.", t.describe())
	return nil
}
