package rules

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Severity says whether a diagnostic stops a rule set from loading.
type Severity uint8

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Diagnostic is one problem found in rule source. The audience is a fraud
// analyst, so every diagnostic points at the exact text it is about, says
// what is wrong in plain words, and, where one exists, says what to write
// instead.
type Diagnostic struct {
	Severity Severity
	Span     Span
	Msg      string // what is wrong, as a sentence
	Hint     string // what to do about it; may be empty
	Line     string // the full source line Span starts on, for rendering
}

// Message is Msg followed by Hint.
func (d Diagnostic) Message() string {
	if d.Hint == "" {
		return d.Msg
	}
	return d.Msg + " " + d.Hint
}

// Error returns the one-line form, "line 1, column 10: <message>".
func (d Diagnostic) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", d.Span.Start.Line, d.Span.Start.Col, d.Message())
}

// Render returns the source line, a caret line underlining the span, and the
// message:
//
//	block if :card_txn_cnt_1h: >= 8
//	         ^^^^^^^^^^^^^^^^^
//	unknown attribute :card_txn_cnt_1h:. Did you mean :card_txn_count_1h:?
func (d Diagnostic) Render() string {
	var b strings.Builder
	b.WriteString(d.Line)
	b.WriteByte('\n')
	b.WriteString(caretLine(d.Line, d.Span))
	b.WriteByte('\n')
	b.WriteString(d.Message())
	return b.String()
}

// caretLine underlines span within line. Tabs before the span are copied so
// the carets land under the same columns in a terminal. A zero-width span
// (for example "expected an expression" at the end of the line) still gets
// one caret, and a span running past the line is cut at its end.
func caretLine(line string, sp Span) string {
	startCol := sp.Start.Col
	width := sp.End.Col - sp.Start.Col
	if sp.End.Line != sp.Start.Line {
		width = utf8.RuneCountInString(line) + 1 - startCol
	}
	if width < 1 {
		width = 1
	}
	var b strings.Builder
	col := 1
	for _, r := range line {
		if col >= startCol {
			break
		}
		if r == '\t' {
			b.WriteByte('\t')
		} else {
			b.WriteByte(' ')
		}
		col++
	}
	for ; col < startCol; col++ {
		b.WriteByte(' ')
	}
	b.WriteString(strings.Repeat("^", width))
	return b.String()
}

// Diagnostics is a list of diagnostics in source order. As an error it
// reports every entry, so an analyst fixes a whole rule set in one pass
// instead of one typo per save.
type Diagnostics []Diagnostic

func (ds Diagnostics) Error() string {
	if len(ds) == 0 {
		return "no errors"
	}
	msgs := make([]string, len(ds))
	for i, d := range ds {
		msgs[i] = d.Error()
	}
	return strings.Join(msgs, "\n")
}

// Render formats every diagnostic with a position header and carets:
//
//	error at line 2, column 10:
//	block if :card_txn_cnt_1h: >= 8
//	         ^^^^^^^^^^^^^^^^^
//	unknown attribute :card_txn_cnt_1h:. Did you mean :card_txn_count_1h:?
func (ds Diagnostics) Render() string {
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s at line %d, column %d:\n", d.Severity, d.Span.Start.Line, d.Span.Start.Col)
		b.WriteString(d.Render())
	}
	return b.String()
}

// HasErrors reports whether any entry is an error rather than a warning.
func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Err returns ds as an error if it holds at least one error, else nil. Use it
// instead of assigning ds to an error directly, which would make an empty
// list a non-nil error.
func (ds Diagnostics) Err() error {
	if !ds.HasErrors() {
		return nil
	}
	return ds
}

// Warnings returns the entries that are warnings.
func (ds Diagnostics) Warnings() Diagnostics {
	var out Diagnostics
	for _, d := range ds {
		if d.Severity == SeverityWarning {
			out = append(out, d)
		}
	}
	return out
}

func (ds Diagnostics) sort() {
	sort.SliceStable(ds, func(i, j int) bool {
		return ds[i].Span.Start.Offset < ds[j].Span.Start.Offset
	})
}

// suggest returns the candidate closest to name by edit distance, or "" if
// none is close enough to be a plausible typo. Comparison ignores case,
// since :Amount: for :amount: is the commonest slip of all.
//
// The cutoff grows with the name's length: one edit in a short name is a
// different word, but two or three in card_txn_count_24h are a typo.
func suggest(name string, candidates []string) string {
	lname := strings.ToLower(name)
	maxDist := 1 + utf8.RuneCountInString(name)/5
	if maxDist > 3 {
		maxDist = 3
	}
	best, bestDist := "", maxDist+1
	for _, c := range candidates {
		d := editDistance(lname, strings.ToLower(c))
		if d < bestDist || (d == bestDist && c < best) {
			best, bestDist = c, d
		}
	}
	if best == "" {
		// A name that contains, or is contained in, exactly one candidate is
		// still worth offering: "email_domain" -> purchaser_email_domain is
		// many edits away but obviously related.
		var hits []string
		for _, c := range candidates {
			lc := strings.ToLower(c)
			if len(lname) >= 4 && (strings.Contains(lc, lname) || strings.Contains(lname, lc)) {
				hits = append(hits, c)
			}
		}
		if len(hits) == 1 {
			return hits[0]
		}
	}
	return best
}

// editDistance is the optimal string alignment distance (Damerau-Levenshtein
// restricted to non-overlapping transpositions) over runes. Transpositions
// count as one edit because swapped letters ("cuont") are how people type.
func editDistance(a, b string) int {
	s, t := []rune(a), []rune(b)
	// Three rolling rows: two back for transpositions, one back, current.
	prev2 := make([]int, len(t)+1)
	prev := make([]int, len(t)+1)
	cur := make([]int, len(t)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(s); i++ {
		cur[0] = i
		for j := 1; j <= len(t); j++ {
			cost := 1
			if s[i-1] == t[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && s[i-1] == t[j-2] && s[i-2] == t[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(t)]
}
