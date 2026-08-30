// Package rules implements RiskGate's fraud rule language: a lexer, a Pratt
// parser, a printer, a type checker whose error messages are written for
// fraud analysts, a compiler to Go closures for online scoring, and a
// generator of random well-typed rules for differential testing.
//
// # The language
//
// A rule set has one rule per line:
//
//	allow  if :purchaser_email_domain: in @trusted_domains
//	block  if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:
//	review if :product_code: = "C" and is_missing(:device_info:)
//	shadow block if :risk_score: >= 80   # evaluated and logged, never enforced
//
// Attributes are :name: (see schema.Catalog), named lists are @name, text is
// "double quoted", and numbers are plain decimals. Conditions combine with
// and, or, not and parentheses; values compare with = != < <= > >=, test
// membership with in (or not in) against a named list or a literal
// ["a", "b"] list, and combine arithmetically with + - * /. The functions
// are is_missing(x), lower(text) and starts_with(text, prefix). Parse
// documents the precedence; in short `a or b and c` is `a or (b and c)`.
//
// # Evaluation order
//
// RuleSet.Evaluate follows the order Stripe documents for Radar: allow rules
// first, and an allowed payment is not evaluated against block or review;
// then block; then review; and a payment no rule matches is allowed. Shadow
// rules are evaluated on every payment and reported, but never decide.
//
// # Missing values
//
// Much of the data is null, so missing is a first-class value: NaN for a
// number and "" for text (see schema.Row). The rules are:
//
//  1. Any comparison or in involving a missing value is false. That
//     includes != : "missing != 5" is false, not true.
//  2. Arithmetic with a missing operand is missing, and so is division by
//     zero.
//  3. lower(missing) is missing; starts_with with either side missing is
//     false; is_missing(x) is true exactly when x is missing.
//  4. and, or and not are ordinary two-valued logic over the results.
//
// The consequence to know is rule 4 meeting rule 1: not flips a comparison
// that was false because of a missing value into true. With :amount:
// missing, `not :amount: > 100` is true while `:amount: <= 100` is false:
//
//	:amount:   :amount: > 100   not :amount: > 100   :amount: <= 100   is_missing(:amount:)
//	150        true             false                false             false
//	50         false            true                 true              false
//	missing    false            true                 false             true
//
// Likewise `not :x: = "a"` matches a missing :x:, while `:x: != "a"` does
// not. So a rule matches a payment with a missing field only when its author
// wrote not, or is_missing, which is the point: comparisons never match
// absent data by accident, and a rule that wants to catch absent data says
// so.
//
// The alternative, SQL-style three-valued logic, would make `not` of an
// unknown comparison also unknown, so neither `x > 100` nor `not x > 100`
// would ever match a missing x. That surprises analysts writing exclusions
// ("block unless the score is high" would silently skip every unscored
// payment), and it needs two bitmaps per node in a vectorized evaluator
// where two-valued logic needs one. With two values, and/or/not keep every
// law of Boolean algebra (De Morgan included); what does not hold is
// `not (a < b)` == `a >= b`, which the table above shows and which is why
// the checker never rewrites one into the other.
//
// # For other evaluators
//
// The backtester evaluates checked ASTs over columns. Everything it needs is
// on the nodes after Check: Attr.Field gives the kind and row slot, Call.Fn
// the builtin, In.Kind and In.Strings/In.Numbers the resolved, deduplicated
// list, and Type the static type of any node. lower is strings.ToLower. The
// semantics above are the whole contract; GenerateExpr and GenerateRow
// produce inputs for checking one evaluator against the other.
package rules
