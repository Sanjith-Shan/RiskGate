package rules

import (
	"unicode"
	"unicode/utf8"
)

// lower(x) is defined as strings.ToLower(x). Building the lowered string
// allocates whenever x has an upper-case letter, and the online path must
// not allocate, so the compiler never builds it. Instead it compares strings
// byte by byte through byteIter, which yields strings.ToLower(s) lazily.
//
// Two details of strings.ToLower are reproduced exactly, and FuzzLower checks
// the iterator against the real thing:
//   - each byte of invalid UTF-8 becomes U+FFFD (three bytes), because
//     strings.Map decodes it as utf8.RuneError and re-encodes the result;
//   - every other rune becomes the UTF-8 encoding of unicode.ToLower(r).
//
// lower(lower(x)) is compiled as lower(x). That relies on unicode.ToLower
// being idempotent, which TestToLowerIdempotent checks for every rune.

// byteIter yields the bytes of s, or of strings.ToLower(s) when lower is set.
type byteIter struct {
	s      string
	i      int
	lower  bool
	buf    [utf8.UTFMax]byte
	bi, bn int // pending bytes of a multi-byte lowered rune: buf[bi:bn]
}

func (it *byteIter) next() (byte, bool) {
	if it.bi < it.bn {
		b := it.buf[it.bi]
		it.bi++
		return b, true
	}
	if it.i >= len(it.s) {
		return 0, false
	}
	c := it.s[it.i]
	if !it.lower {
		it.i++
		return c, true
	}
	if c < utf8.RuneSelf {
		it.i++
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		return c, true
	}
	r, w := utf8.DecodeRuneInString(it.s[it.i:])
	it.i += w
	it.bn = utf8.EncodeRune(it.buf[:], unicode.ToLower(r))
	it.bi = 1
	return it.buf[0], true
}

// foldEqual reports whether a and b are equal after lowering those whose
// flag is set.
func foldEqual(a string, lowerA bool, b string, lowerB bool) bool {
	if !lowerA && !lowerB {
		return a == b
	}
	x := byteIter{s: a, lower: lowerA}
	y := byteIter{s: b, lower: lowerB}
	for {
		cx, okx := x.next()
		cy, oky := y.next()
		if okx != oky || cx != cy {
			return false
		}
		if !okx {
			return true
		}
	}
}

// foldHasPrefix reports whether s starts with prefix after lowering those
// whose flag is set.
func foldHasPrefix(s string, lowerS bool, prefix string, lowerP bool) bool {
	if !lowerS && !lowerP {
		return len(s) >= len(prefix) && s[:len(prefix)] == prefix
	}
	x := byteIter{s: s, lower: lowerS}
	p := byteIter{s: prefix, lower: lowerP}
	for {
		cp, ok := p.next()
		if !ok {
			return true
		}
		cx, ok := x.next()
		if !ok || cx != cp {
			return false
		}
	}
}

// lowerInto writes strings.ToLower(s) into buf and reports its length, or
// false if it does not fit.
func lowerInto(buf []byte, s string) (int, bool) {
	it := byteIter{s: s, lower: true}
	n := 0
	for {
		c, ok := it.next()
		if !ok {
			return n, true
		}
		if n == len(buf) {
			return 0, false
		}
		buf[n] = c
		n++
	}
}
