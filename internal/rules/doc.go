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
// number and "" for text (see schema.Row). Conditions are evaluated in
// Kleene's three-valued logic, TRUE, FALSE or UNKNOWN, and a rule matches
// only when its condition is TRUE. That is exactly how SQL evaluates a
// WHERE clause, and it matches Stripe's documentation for Radar, which says
// that a comparison involving a missing value, and not over one, is false.
//
//  1. A comparison, in, or starts_with with a missing operand is UNKNOWN.
//     That includes != : "missing != 5" is UNKNOWN, so it never matches.
//  2. is_missing(x) is TRUE when x is missing and FALSE otherwise, never
//     UNKNOWN. It is how a rule says what it wants done with absent data.
//  3. not UNKNOWN is UNKNOWN. FALSE and anything is FALSE, TRUE or anything
//     is TRUE, and every other and/or involving UNKNOWN is UNKNOWN.
//  4. Arithmetic with a missing operand is missing, and so is division by
//     zero; lower(missing) is missing.
//
// With :amount: missing, a comparison never matches, whichever way it is
// written:
//
//	:amount:   :amount: > 100   not :amount: > 100   :amount: <= 100   is_missing(:amount:)
//	150        TRUE             FALSE                FALSE             FALSE
//	50         FALSE            TRUE                 TRUE              FALSE
//	missing    UNKNOWN          UNKNOWN              UNKNOWN           TRUE
//
// So `not (x > 100)` and `x <= 100` agree on every row, and so do
// `not (x = "a")` and `x != "a"`. A rule matches a payment with a missing
// field only through is_missing, or through an or whose other side is TRUE.
// To include missing values in an exclusion, the author says so:
// `not :amount: > 100 or is_missing(:amount:)`.
//
// Why not two-valued logic, where a comparison with missing is simply false
// and not flips it to true? It is simpler to implement, but it makes
// `not (x > 100)` match every payment with no amount while `x <= 100`
// matches none. An analyst reading the rule cannot see that difference, and
// Radar's documented behavior is the opposite, so a rule tested against one
// and run on the other would silently disagree. Three-valued logic keeps
// the negated and the rewritten forms equal, matches Radar and SQL, and
// still obeys De Morgan's laws. What it gives up is the law of the excluded
// middle: `x > 100 or not x > 100` does not match a missing x.
//
// # For other evaluators
//
// The backtester evaluates checked ASTs over columns. Everything it needs is
// on the nodes after Check: Attr.Field gives the kind and row slot, Call.Fn
// the builtin, In.Kind and In.Strings/In.Numbers the resolved, deduplicated
// list, and Type the static type of any node. lower is strings.ToLower. The
// semantics above are the whole contract; GenerateExpr and GenerateRow
// produce inputs for checking one evaluator against the other.
//
// An evaluator does not need a third value at run time. Asking of each node
// either "is it TRUE?" or "is it FALSE?", with not switching the question
// and a missing operand answering no to both, is Kleene logic exactly;
// compileBool describes the rules.
package rules
