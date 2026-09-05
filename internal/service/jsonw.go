package service

import (
	"math"
	"strconv"
	"unicode/utf8"
)

// The assess response and the decision log are written with these append
// helpers instead of encoding/json. Both are on the payment path (the log is
// encoded off it, but at the same rate), and reflection-based encoding costs
// several allocations per value; appending into a reused buffer costs none.
// The output is ordinary JSON that encoding/json reads back exactly: floats
// use strconv's shortest round-trip form, so a logged feature parses to the
// same float64 bits the service computed, which the audit depends on.

// appendString appends s as a JSON string. Invalid UTF-8 becomes U+FFFD, as
// encoding/json does, so the output is always valid JSON.
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// appendFloat appends v in shortest round-trip form, or null for NaN and the
// infinities, which JSON cannot carry. Missing values are NaN, so null in
// the log means missing.
func appendFloat(b []byte, v float64) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, v, 'g', -1, 64)
}

// appendKey appends "key": after a separating comma unless first.
func appendKey(b []byte, key string, first bool) []byte {
	if !first {
		b = append(b, ',')
	}
	b = append(b, '"')
	b = append(b, key...)
	return append(b, '"', ':')
}
