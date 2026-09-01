// Package backtest answers the question a fraud analyst asks before a rule
// goes live: what would it have done?
//
// # Pieces
//
//   - Table is a column store of feature rows (numbers as float64 with NaN
//     for missing, strings dictionary-coded with code 0 for missing) plus
//     each payment's ID, time, amount and label. Builder appends
//     schema.Rows; Save and Load write and read it as a flat binary file
//     that loads in well under a second at full size.
//   - CompileVector is the rule compiler's second back end. It turns a
//     checked condition into column operators that evaluate 4096 rows at a
//     time into a Bitmap, in parallel across chunks. String predicates are
//     decided once per dictionary entry, not once per row.
//   - EvaluateRuleSet decides every row in Radar's order (allow, then
//     block, then review) and attributes each decision to the first
//     matching rule in source order, exactly as rules.RuleSet.Evaluate
//     does online.
//   - Backtester.Run reports what a proposed rule changes against the rules
//     in force: payments and dollars, fraud and legitimate, precision,
//     share of fraud dollars, overrides for allow rules, overlap with every
//     rule in force, unreachable rules, sample payments, and a plain-English
//     Summary. Backtester.Sweep does the same for every risk_score threshold
//     in one pass.
//   - SimulateLabelTimes and Backtester.CompareLabelDelay are experiment 7:
//     how much a naive backtest over recent payments understates a rule, on
//     SIMULATED dispute arrival times.
//   - DiffTest is experiment 3: the closure evaluator and the vectorized
//     evaluator must agree on every generated rule and every row.
//
// # Label maturity
//
// Fraud labels are disputes, and disputes arrive weeks after the payment.
// By default a backtest leaves out payments made within DefaultMaturity
// (60 days) of its end, and says how many it left out; see DefaultMaturity
// for why 60.
//
// # Dates
//
// TransactionDT is seconds from an unstated reference. Dates in reports use
// DefaultEpoch, 2017-12-01 UTC, the start date commonly assumed for
// IEEE-CIS. That is an assumption, and Options.Epoch changes it.
package backtest
