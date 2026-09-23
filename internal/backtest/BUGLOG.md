# internal/backtest bug log

Every disagreement between the two rule evaluators found during development,
and every other defect found in this package, in the order found. Each entry
says which side was wrong, the root cause, the fix, and the test that now
covers it.

## Harnesses

| Harness | What it checks | Scale |
|---|---|---|
| `TestDifferential` | Generated rules (depth 0 to 6) on four SYNTHETIC tables: generator rows; 15% adversarial values; about 60% missing; about 95% missing. Each twice: generator constants, and constants drawn from the table (`Ground`) | 20,000 rules x 2,000 rows = 40M rule-row checks per `go test` (fewer under `-race`) |
| `TestDifferentialEdgeRules` | 68 hand-written rules aimed at the semantics' sharp edges (below) on three tables salted with adversarial values, one of 130 rows so the partial last bitmap word is exercised | every `go test` |
| `TestEvaluateRuleSetMatchesEvaluate` | Vectorized Radar-order decisions and per-rule attribution against `rules.RuleSet.Evaluate`, 60 generated rule sets of 1 to 25 rules | every `go test` |
| `TestRunMatchesOracle` | Report counts against the definition: evaluate the rule set with and without the proposed rule, row by row, and count the rows whose action differs | 80 random rule sets and windows |
| `backtest difftest` | Same as `TestDifferential`, at full size | 5,000 rules (depth 0 to 8) x 590,540 SYNTHETIC rows = 2.95 billion checks, 0 disagreements, 1m17s on an M3 Pro under heavy load, 2026-09-23 |

Adversarial values: -0, ±5e-324, 1e-300, 0.1+0.2, ±MaxFloat64, ±Inf,
2^53+1; the Kelvin sign (lowers to ASCII `k`), dotted capital I (lowering
adds a byte), capital sharp s, a title-case digraph, Greek capitals, invalid
UTF-8 alone and after text lowering changes, NUL, a lone space. Edge rules
cover `!=` with missing, `not` over missing, constant and per-row division
by zero, NaN propagation, `-0` in lists and comparisons, lists of more and
fewer than 8 items (both evaluators switch strategy there), number formatting
in lists (`100` vs `100.0`, `0.30000000000000004`), `lower` on non-ASCII and
invalid UTF-8, `starts_with` in every constant/column combination, two string
columns compared through lowered views, and constant-folded conditions.

## Is the harness able to find anything?

No disagreement between the evaluators turned up during development: the
first run of the differential test passed, and so did every run since. A
test that has never failed proves little on its own, so each harness was
checked by planting a bug in the vectorized evaluator and confirming the
tests catch it (all reverted):

| Planted bug | Caught by | Disagreements |
|---|---|---|
| numeric `!=` true for a missing value | `TestDifferential` | about 30,000 over about 110 rules per table |
| `lower()` ignored (raw dictionary used) | `TestDifferential` | about 22,000 over about 100 rules |
| division by zero gives ±Inf instead of missing | `TestDifferential` | about 3,800 over about 70 rules |
| string predicates true for missing (code 0) | `TestDifferential` | about 60,000 over about 220 rules |
| `not` leaves bits set past the last row | `TestDifferentialEdgeRules` (130-row table) | failed |
| sparse refinement inverts a numeric comparison | `TestDifferential` | about 400 over 8 rules |
| sparse refinement inverts a string lookup | `TestDifferential` | about 1,000 over 20 rules |
| column-vs-column string refinement ignores missing | `TestDifferential` | about 15 over 1 to 5 rules |

Forcing sparse refinement on every chunk (a change that should be invisible)
passes, as it should.

## Defects found in this package

### 1. Report said "0 payments worth $0" for an unreachable rule
- **Side:** backtest report (not an evaluator).
- **Found by:** reading summaries of hand-built cases.
- **Cause:** the summary always led with the changed-payment sentence, so a
  review rule fully covered by a block rule read "would have sent 0 payments
  worth $0 to review. It matches 1 payment, but ...".
- **Fix:** an unreachable rule gets its own lead sentence: "this rule matches
  N payments, but the rules in force already decide all of them, so it would
  change nothing."
- **Test:** `TestSummaryVoice`, `TestRunHandExample`.

### 2. Singular and plural mismatches in summaries
- **Side:** backtest report.
- **Found by:** reading summaries on small tables.
- **Cause:** "1 payment of these is blocked today", "1 payment ... was
  excluded because their disputes ...", "100% of them was", and "0% of them
  were later disputed as fraud, which is 0% of all fraud dollars" for an
  all-legitimate result.
- **Fix:** separate sentences for exactly one payment ("It was later disputed
  as fraud"), for none ("None of them were later disputed as fraud"), and for
  all of the overrides ("All of them are blocked today"); "its dispute" versus
  "their disputes".
- **Test:** `TestSummaryVoice`, `TestRunHandExample`.

### 3. The default window counted 183 days of a 182-day table
- **Side:** backtest report.
- **Cause:** `Options.From == 0` meant TransactionDT 0, but IEEE-CIS (and the
  synthetic tables) start at 86400, so "over the N days" included a day with
  no data.
- **Fix:** zero bounds mean the table's own first and last payments.
- **Test:** `TestRunHandExample` (explicit window), `TestRunOptions`.

### 4. Dead code: refinement of a constant condition
- **Side:** vectorized evaluator (no wrong answers).
- **Found by:** the coverage report showed it never ran.
- **Cause:** `and`/`or` fold constant operands away at compile time, exactly
  as the closure compiler does, so an and-node never has a constant side to
  refine.
- **Fix:** removed.

### 5. Table load touched every byte twice
- **Side:** table file (performance, no wrong answers).
- **Found by:** `BenchmarkLoad` profile: most time in page faults and memory
  clearing.
- **Cause:** the file was read into one buffer and then decoded value by value
  into freshly allocated columns.
- **Fix:** read each column straight into its own memory (a byte view of the
  slice; byte-swapped on big-endian machines), checksumming as it streams.
  About 2.5 times faster under the same machine load.
- **Test:** `TestSaveLoadRoundTrip` (bit-exact, including NaN and -0),
  `TestLoadRejectsBadFiles`.

## Findings about `internal/rules`

None. No input found a disagreement, and so no case where the closure
evaluator departs from `rules/doc.go`.
