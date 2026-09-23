# Interview defense

Short answers to the ten questions RiskGate has to survive before it goes on a resume, each tied to the code that backs it. Line numbers are as of 2026-09-23 and will drift, so the function names are the stable reference. Any answer that needs a measured number says `TBD` until the experiment is run. Do not fill one in from memory.

## 1. Point-in-time correctness, and the leakage test

**The claim.** A payment's features use only payments strictly before it. Otherwise training sees the future, the offline metrics are inflated, and the model is worse online than it looked.

**How it is enforced.**
- One total order, `(TransactionDT, TransactionID)`, in `data.Less` (`internal/data/txn.go:30`). IEEE-CIS has many payments in the same second, so "before" needs a tie-break. A larger id at the same second counts as later. `data.CheckSorted` (`internal/data/load.go:206`) rejects a repeated pair, because it would make the order ambiguous.
- Score before update. `Engine.ScoreAndUpdate` (`internal/features/engine.go:171`) reads the state, fills the row, then adds the payment, under one per-key lock. A payment never counts itself.
- `features.Replay` (`internal/features/replay.go:18`) checks the order as it goes and fails loudly, since unsorted input would leak silently.

**The test.** `TestLeakage` (`internal/features/leakage_test.go:35`). For a sampled payment *i*, rewrite history after *i* by mutating or deleting every later event and inserting new ones (same second with a larger id, same card, same device). Shuffle the whole input, run the export's pipeline (`data.Sort` then `Replay`), and require *i*'s features and everything before it to be **bit-identical** to the clean run. It runs for exact, bucketed and sketch state. CI runs a small sample under `-race`, and `RISKGATE_LEAKAGE_SAMPLES` and `RISKGATE_LEAKAGE_DATA` scale it up.

**Why this test and not a code review.** It checks the property, not the mechanism. If someone later adds a feature that peeks ahead, the three mechanisms above could all still look fine and the test would still fail.

**Follow-up to expect: "what about online, out of order?"** Windows are in event time (`created`), not wall-clock time. A late event is recorded at the key's latest time (`TestLateEventIsClamped`), so it is counted and history stays ordered. The trade is that it can stay in a window a few seconds too long, and I chose that over buffering every payment.

## 2. Train/serve skew, and the two parity results

**Skew** is the model being trained on features computed one way and served on features computed another. It fails silently, because nothing crashes and the model just gets worse.

**Ruled out by construction.** One feature implementation. `Engine.ScoreAndUpdate` is called by the export (through `Replay`), and the backtester's table build and the service are designed to call it the same way. Check the state of those two before claiming them as done. Python never computes a feature (`python/riskgate.py` module docstring). Even the CSV read uses `float_precision="round_trip"`, because pandas' default parser can be one ulp off. Unknown or mistyped `risk_fields` keys are errors (`data.ParseRiskFields`), since a typo would otherwise turn into a missing value online only.

**Proved by two results.**
1. **Features.** Replay the test month through the HTTP service and compare every feature vector with the offline export. Target zero mismatches. Result `TBD (experiment 2)`.
2. **Model.** Go raw scores against LightGBM's `predict(raw_score=True)` on every test-month row (`cmd/parity`). Target bit-identical. Result `TBD (experiment 2)`.

Together they answer "how do you know the model in production is the model you evaluated". Features are the same, and the function of the features is the same.

**Details worth having ready.**
- Bit-identical is possible because `PredictRaw` (`internal/model/predict.go:14`) sums trees in file order into a float64 starting at 0, like LightGBM's `GBDT::PredictRaw`. Summation order is the usual source of last-bit differences.
- `kZeroThreshold` is `1e-35f` in LightGBM, a float literal, so its double value is about `1.0000000180025095e-35`. `internal/model/lightgbm.go:39` spells it `float64(float32(1e-35))`, and `python/gen_fixtures.py` probes values between the two.
- Calibration is `np.interp` semantics written step by step in both languages (`Calibrator.Apply`, `internal/model/calibrate.go:72`, and `riskgate.interpolate`). NumPy's own `np.interp` may fuse a multiply-add depending on how it was built. In Go the `float64(...)` conversion forbids fusion.

## 3. Why the model feeds the rules instead of deciding

- **Merchants tune rules, not weights.** A threshold on `:risk_score:` or an allow rule is something a fraud analyst can change and backtest. A model is not.
- **Merchants know things the model does not.** Trusted partners, expected spikes, products they never ship somewhere. Rules carry that.
- **Explainable decisions.** The log says which rule decided and why, which support can repeat to a customer.
- **Blast radius.** A bad rule is one line to revert, and shadow mode shows its live effect before it is enforced. A bad model affects everything.

This is also Radar's design, where the risk score is an attribute rules read. `risk_score` is a calibrated probability in percent, not a percentile (`model.RiskScore`, `internal/model/calibrate.go:105`). So `>= 85` means the same thing tomorrow even if traffic shifts, as long as the calibration holds.

## 4. Radar's evaluation order and the missing-value semantics

**Order** (`RuleSet.Evaluate`, `internal/rules/ruleset.go:67`). Allow first, and an allowed payment skips block and review. Then block, and a blocked payment skips review. Then review. No match means allow. Radar puts request-3DS rules before all of these, and RiskGate leaves 3DS out. Within one action Radar leaves rules unordered. RiskGate reports the first match in source order so the logged rule is deterministic.

**Defending the order.** It makes rules composable. An allow rule wins over every block rule, present and future, without the author reading them, and adding a block rule can never un-block anything. The cost is that a careless allow rule lets fraud through, which is why the backtester reports overrides for allow rules (`Report.OverridesBlock`, `internal/backtest/report.go`).

**Missing values** (`internal/rules/doc.go`). Kleene three-valued logic, and a rule matches only when its condition is TRUE, the same as a SQL `WHERE`.
1. A comparison, `in` or `starts_with` with a missing operand is UNKNOWN, including `!=`.
2. `not UNKNOWN` is UNKNOWN. FALSE decides an `and`, TRUE decides an `or`.
3. `is_missing(x)` is never UNKNOWN. It is how a rule asks for absent data.
4. Arithmetic with missing is missing, and so is division by zero.

The consequence is that `not :amount: > 100` and `:amount: <= 100` agree: neither matches a missing amount.

**Defending it.** It matches Radar, whose docs say `NOT` over a comparison with a missing feature "always returns false", and it matches SQL, so neither analysts nor engineers are surprised. De Morgan still holds. What it gives up is the excluded middle: `x > 100 or not x > 100` skips a missing `x`.

**The story worth telling.** The first version was two-valued, and `not` flipped a missing comparison to true. Checking the docs against Radar found the mismatch. The fix switched both evaluators, the differential test showed they still agreed on every row, and a planted "two-valued `not`" bug was caught right away. Neither evaluator needs a third value at run time: each node answers "is it TRUE?" or "is it FALSE?", `not` switches the question, and missing answers no to both.

## 5. What a Pratt parser does with `a or b and c`

Every operator has a binding power. Here, loosest first, that is `or`, `and`, `not`, comparisons and `in`, `+ -`, `* /`, unary minus (`internal/rules/parser.go:19`).

`parseExpr(minPrec)` parses an operand, then loops. While the next operator binds tighter than `minPrec`, it consumes the operator and parses the right side with `parseExpr(thatOperator'sPrec)`, so the right side only takes operators that bind tighter still.

Walk-through. The top call parses `a` and sees `or`, which is above the lowest level, so it parses the right side at `or`'s level. That call parses `b` and sees `and`, which binds tighter than `or`, so it keeps going and returns `b and c`. The result is `a or (b and c)`, same as `1 + 2 * 3`. Left associativity falls out because the loop continues at the same level. The loop is around `internal/rules/parser.go:260-305`.

Extras worth mentioning.
- Comparisons do not chain. The loop remembers it just parsed one and rejects `1 < :amount: < 5` with a fix.
- `x not in @list` is the one infix use of `not`, handled by peeking one token ahead.
- `TestPrecedence` (`internal/rules/parser_test.go:40`) pins the table, and `FuzzParse` checks `parse(print(ast)) == ast` with minimal parentheses.
- Why Pratt over recursive descent with a function per level. It is the same thing with the levels in a table, so adding an operator is one table entry.

## 6. The three velocity-state designs, and which ships

| | Exact | Bucketed ring | Sketch |
|---|---|---|---|
| Structure | per-key deque of events, running totals | per-key, per-window ring of time buckets, only non-empty ones stored | count-min per time bucket, HyperLogLog for distinct cards, min/max sketches for first/last seen |
| Memory | grows with traffic, unbounded under attack | bounded per key | fixed, independent of key count |
| Error | none, the reference | trailing edge only, never undercounts, overcounts by at most one bucket | never undercounts counts, collision error grows with total traffic |
| Code | `NewExact`, `internal/features/exact.go:13` | `NewBucketed`, `internal/features/bucketed.go:44` | `NewSketch`, `internal/features/sketch.go:103` |

Concurrency is `NewSharded` (N shards, each an RWMutex padded to a 128-byte cache line), compared against one mutex and `sync.Map` (`internal/features/concurrent.go`). Hashing is FNV-1a with a finalizer, not `maphash`, because shard and sketch positions go into snapshots and must survive a restart.

**Which ships, with numbers.** `TBD (experiments 4 and 5)`. The answer has to include bytes per key, ns per update and read, error against exact, the effect on recall at 1% FPR, and the shard count that held p99 inside the deadline. Do not answer this question until those are measured.

## 7. Saabas contributions, and how they differ from SHAP

**Saabas** (`Model.Contributions`, `internal/model/saabas.go:31`). Follow the payment's path through each tree. At each step from a node to a child, credit the change in the node's expected value (LightGBM's `internal_value`) to the feature that node split on. A tree's credits telescope to leaf minus root, so bias plus contributions equals the raw score (`TestSaabasSumsToRawScore`). It costs about one prediction and does not allocate.

**TreeSHAP** (Lundberg, Erion and Lee, 2018) averages a feature's marginal contribution over all orders features could be revealed in. That gives consistency, meaning a model that relies more on a feature never gives it less credit, and fair treatment of interactions.

**The difference in one sentence.** Saabas credits only the one order the tree happens to test features in, so it is path-dependent and biased toward features split near the root, and the Lundberg paper names it as inconsistent.

**Why Saabas is fine here.** A reason string only asks which features pushed this score up. RiskGate never uses the contributions for global importance or anything that needs consistency. LightGBM's `pred_contrib` is TreeSHAP and will not match, and nothing in the repo calls these SHAP values.

## 8. Label delay, and why a naive backtest lies

Fraud labels come from disputes, and disputes arrive weeks after the payment, up to the networks' 120-day limit. A backtest over the last 30 days sees only the disputes that have already arrived. It **undercounts fraud**, so a block rule looks less precise than it is and an allow rule looks safer than it is. That is the dangerous direction.

**The fix.** Exclude payments younger than a maturity window and say so in the summary. The default is 60 days (`DefaultMaturity`, `internal/backtest/report.go:28`).

**The honest part.** IEEE-CIS has no label arrival times. Experiment 7 **simulates** them from an assumed lognormal delay, median 30 days, sigma 0.5, capped at 120 days (`DefaultDelay`, `internal/backtest/labeldelay.go:35`). Under that assumption about 92% of disputes arrive within 60 days. The draw depends only on the seed and the payment id, so it is stable under filtering. Every number from it is labelled simulated. Understatement of the naive backtest `TBD (experiment 7)`.

## 9. Fail open against fail closed, with the count from experiment 9

**Assess fails open.** If RiskGate is down or past the deadline, Clearinghouse lets the payment through as `risk_unavailable`. The reasoning is that a fraud check filters revenue, and failing closed turns a RiskGate outage into a total payment outage when most payments are legitimate.

**The cost.** Every payment in the outage is unchecked, and an attacker who can degrade RiskGate gets a window. Experiment 9 counts it. Payments that went through unchecked `TBD (experiment 9)`, of which fraud `TBD (experiment 9)`, with Clearinghouse's p99 during the outage `TBD (experiment 9)`.

**Webhook verification fails closed.** A verifier with no secrets or an empty secret accepts nothing and the service refuses to boot (`Verifier.Validate`, `internal/webhook/signature.go:216`, and `VerifyAt` at line 229). HMAC with an empty key is computable by anyone, and a forged dispute would poison the labels. So the same system makes opposite choices in two places, and each follows from what a failure costs there.

**What would change the answer.** For a high-risk merchant, or for payment amounts above some limit, fail closed or fall back to a static rule set held on Clearinghouse's side. That is a policy knob, not a rewrite.

## 10. What `uid` is, where it came from, and why it is an approximation

**What.** An approximate customer identity. It is `card1`, `addr1`, and the day the card was first used, computed as `floor(TransactionDT / 86400) - D1` (`features.UIDKey`, `internal/features/key.go:87`). `D1` is days since the card's first transaction, so subtracting it from today's day gives a constant per card.

**Where from.** The IEEE-CIS first-place team, Chris Deotte and Konstantin Yakovlev (FraudSquad). Their write-up names "the three columns card1, addr1, and D1" as the key to identifying clients, and they used `uid` to build group aggregates rather than as a feature. RiskGate does the same. It is an entity key for velocity features, not a model input. Credited in DESIGN.md and in the code comment.

**Why approximate.**
- Two customers with the same `card1` (coarse, many share it) and region and first-use day collide.
- One customer splits into several when `D1` is missing, clipped, or drifts, or when they change billing address.
- If any part is missing the key is missing, and that payment gets missing `uid` features rather than being pooled with every other missing-key payment (`features.KeysOf`).
- It is reverse-engineered from anonymized columns. It is not ground truth, and reasons phrase it as "this customer" only because a merchant would.

Related, and worth saying unprompted. The `email` entity is the purchaser email **domain**, because that is all the data has. So `distinct_cards_per_email_24h` is a domain-level signal and on `gmail.com` it mostly measures traffic.
