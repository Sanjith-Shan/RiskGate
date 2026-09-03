# internal/rules bug log

Real bugs found by fuzzing and property tests, in the order they were found.
Each has a regression test and, where the fuzzer found it, a seed in
`testdata/fuzz/FuzzParse`.

## Harnesses

| Harness | What it checks | Local run |
|---|---|---|
| `FuzzParse` | Arbitrary text: no panics; every parsed rule prints to text that parses back to an equal tree, and printing is a fixed point; every rule that loads evaluates the same as the reference interpreter | 60 s, 3.55M execs (after fixes 1 and 2) |
| `FuzzGeneratedRoundTrip` | Fuzzer-chosen generator seed and depth: print/parse round trip of generated trees; compiled closures against the reference interpreter on generated rows | 60 s, 4.07M execs |
| `FuzzLower` | The allocation-free lowering helpers against `strings.ToLower`, on arbitrary bytes including invalid UTF-8 | 60 s, 8.93M execs |
| `TestCompiledMatchesReference` | 3,000 generated rules x 200 generated rows, compiled against the reference interpreter | every `go test` |
| `TestPrintRoundTripGenerated` | 5,000 generated trees, depth 1 to 6 | every `go test` |
| `TestZeroAllocs` | 300 generated rules evaluate with 0 allocations | every `go test` |

Machine: Apple M3 Pro, 12 cores, Go 1.26.5, 2026-09-23.

## Semantics change: three-valued logic (2026-09-23)

Conditions now use Kleene's three-valued logic, collapsed to "matches only
if TRUE", to match Radar's documented treatment of `not` over missing
values (see `doc.go`). The closure compiler answers "is it TRUE?" or "is it
FALSE?" for each node, and `not` switches the question. The reference
interpreter in `compile_test.go` was rewritten as a separate check: it
carries an explicit third value through Kleene's tables. The change turned
up no bugs. Harness runs after it:

- `FuzzParse`: 30s, 3.16M execs.
- `FuzzGeneratedRoundTrip`: 30s, 0.90M execs.
- `FuzzLower`: 30s, 2.47M execs.
- All three used `-fuzzminimizetime 2s`, and none found a failure.

A planted bug made `not` two-valued again (`!TRUE(x)` in place of
`FALSE(x)`). Three tests caught it: `TestMissingSemantics`,
`TestEvaluate` and `TestCompiledMatchesReference`. `FuzzGeneratedRoundTrip`
caught it in under a second. The bug was then reverted.

## 1. Checking was quadratic in nesting depth

- **Found by:** `FuzzParse`. Throughput fell to 0 execs/s and the run
  failed with `context deadline exceeded`. The Go fuzzer does not save hangs,
  so the input was found by timing adversarial shapes.
- **Input:** `block if is_missing(is_missing(...(:amount:)...))`, 20,000
  levels deep, took 7.0 s to load.
- **Cause:** every level reported "is_missing needs a value, but X is a
  true/false condition", and X was the printed subtree, so the checker
  printed O(n^2) bytes. The call also returned `bool` after the error, so
  the next level up reported the same error again.
- **Fix:** `describe` stops walking after 8 nodes and says "this
  expression" for anything larger, and a call with a bad argument returns
  `TypeInvalid` so its parent stays quiet. The same input now loads in 58 ms.
- **Regression test:** `TestNestedErrorsStayBounded`; seed
  `testdata/fuzz/FuzzParse/nested_is_missing`.

## 2. Junk input produced one diagnostic per byte

- **Found by:** `FuzzParse`, which was still stalling after fix 1. The
  slowest corpus entries were 4 KB lines of control characters.
- **Input:** a line such as `0A000...` followed by 4,000 `\x7f` bytes, or
  4,000 `'` characters.
- **Cause:** the lexer reported each unexpected byte as its own error, and
  every rendered diagnostic quotes the full line, so rendering was O(n^2)
  in line length. Hundreds of carets for one pasted blob is also useless to
  an analyst.
- **Fix:** a run of adjacent unexpected characters is one diagnostic
  covering the run, and the lexer keeps at most 3 errors per line.
- **Regression test:** `TestLexErrorsBounded`; seed
  `testdata/fuzz/FuzzParse/junk_line`.

## Checked and found correct

These are the properties most likely to break. The harnesses above check
each of them, and none has failed:

- **Round trip.** `parse(print(ast)) == ast` holds with minimal parentheses.
  That covers `not` under comparisons, chained unary minus (`--1`), and
  `x not in l`, which prints as `not x in l`.
- **Folding.** Constant folding and the attribute-against-constant fast
  paths agree with the reference interpreter. That includes `!=` against
  missing, a constant zero divisor, and `lower` on non-ASCII text and
  invalid UTF-8.
- **Idempotent `lower`.** `unicode.ToLower` is idempotent for every rune
  (`TestToLowerIdempotent`), so compiling `lower(lower(x))` as `lower(x)` is
  sound.
