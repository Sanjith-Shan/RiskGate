# Interview defense

Short answers to the questions RiskGate has to survive, each tied to the code and the result that backs it. Line numbers are as of 2026-09-24 and will drift, so the function names are the stable reference. Every number here comes from a file under `results/`, and the file is named next to it. Experiments 8 and 9 have not been run, and their answers say so.

## The numbers, in one place

| What | Number | Source |
|---|---|---|
| Recall at 1% FPR, raw columns → raw plus velocity (test month, touched once) | 14.8% → 16.8% | `results/ieee/test_metrics.md` |
| Fraud-dollar recall at a 1% legit-dollar budget | 15.6% → 20.5% | same |
| ROC-AUC | 0.7745 → 0.8105 | same |
| Payments replayed through the HTTP service against the offline pipeline | 590,540, 0 mismatches | `results/exp2/exp2.md` |
| Go evaluator against LightGBM, test month, 954 trees | 92,427 / 92,427 bit-identical | same |
| Closure against vectorized evaluator | 10,000 rules × 590,540 rows, 0 disagreements, on two tables | `results/difftest_real/README.md` |
| Sketch cost on validation ROC-AUC, before and after the first-seen fix | 0.0137, then 0.0024 | `results/state_shootout/README.md` |
| Naive backtest understatement of fraud dollars (**simulated** delays) | 80% to 88%, 4% to 6% with 60-day maturity | `results/label_delay/README.md` |
| `rules/default.rules` on validation | 39.1% precision, 31.1% recall, 1.71% FPR, 35.7% fraud-dollar recall | `results/rules_default/TUNING.md` |

Latency (experiment 5) and backtest speed (experiment 6) were measured on a machine with a load average between 10 and 66. I do not quote them as results. If asked, I say the run exists, that it is not quotable, and why.

## 1. Point-in-time correctness, and the leakage test

**The claim.** A payment's features use only payments strictly before it. Otherwise training sees the future, the offline metrics are inflated, and the model is worse online than it looked.

**How it is enforced.**
- One total order, `(TransactionDT, TransactionID)`, in `data.Less` (`internal/data/txn.go:30`). IEEE-CIS has many payments in the same second, so "before" needs a tie-break. A larger id at the same second counts as later. `data.CheckSorted` (`internal/data/load.go:206`) rejects a repeated pair, because it would make the order ambiguous.
- Score before update. `Engine.ScoreAndUpdate` (`internal/features/engine.go:171`) reads the state, fills the row, then adds the payment, under one per-key lock. A payment never counts itself.
- `features.Replay` (`internal/features/replay.go:18`) checks the order as it goes and fails loudly, since unsorted input would leak silently.

**The test.** `TestLeakage` (`internal/features/leakage_test.go:35`). For a sampled payment *i*, rewrite history after *i* by mutating or deleting every later event and inserting new ones (same second with a larger id, same card, same device). Shuffle the whole input, run the export's pipeline (`data.Sort` then `Replay`), and require *i*'s features and everything before it to be **bit-identical** to the clean run. It runs for exact, bucketed and sketch state. CI runs it twice, 12 samples under `-race` and 48 in the coverage job, and `RISKGATE_LEAKAGE_SAMPLES` and `RISKGATE_LEAKAGE_DATA` scale it up to a full dataset.

**Why this test and not a code review.** It checks the property, not the mechanism. If someone later adds a feature that peeks ahead, the three mechanisms above could all still look fine and the test would still fail.

**Follow-up to expect, "what about online, out of order?"** Windows are in event time (`created`), not wall-clock time. A late event is recorded at the key's latest time (`TestLateEventIsClamped`), so it is counted and history stays ordered. The trade is that it can stay in a window a few seconds too long, and I chose that over buffering every payment.

## 2. Train/serve skew, and the two parity results

**Skew** is the model being trained on features computed one way and served on features computed another. It fails silently, because nothing crashes and the model just gets worse.

**Ruled out by construction.** One feature implementation. `Engine.ScoreAndUpdate` is called by the export (through `Replay`), by `riskgate table` for the backtester, and by the service once per request. Python never computes a feature (`python/riskgate.py` module docstring). Even the CSV read uses `float_precision="round_trip"`, because pandas' default parser can be one ulp off. Unknown or mistyped `risk_fields` keys are errors (`data.ParseRiskFields`), since a typo would otherwise turn into a missing value online only.

**Proved by two results** (`scripts/experiments/exp2.sh`, `results/exp2/exp2.md`).
1. **Features.** A fresh `riskgate serve` got all 590,540 payments over HTTP, one at a time in `(TransactionDT, TransactionID)` order. `cmd/serveparity compare` checked every decision-log line bit for bit, all 53 features against a fresh offline replay, the 53 encoded inputs against `export.csv`, and the raw score, probability and `risk_score` against the offline Scorer. **0 mismatches** on every check, and `riskgate audit` replayed all 590,540 decisions identically.
2. **Model.** Go raw scores against LightGBM's `predict(raw_score=True)` on every test-month row (`cmd/parity`). **92,427 of 92,427 bit-identical**, max difference 0.

A zero is only worth something if the check can fail. The negative control moved one amount by one ulp (68.5 to 68.50000000000001) and deleted one log line, and the compare reported both and exited 1 (`results/exp2/negative_control.json`).

**Details worth having ready.**
- Bit-identical is possible because `PredictRaw` (`internal/model/predict.go:14`) sums trees in file order into a float64 starting at 0, like LightGBM's `GBDT::PredictRaw`. Summation order is the usual source of last-bit differences.
- `kZeroThreshold` is `1e-35f` in LightGBM, a float literal, so its double value is about `1.0000000180025095e-35`. `internal/model/lightgbm.go:39` spells it `float64(float32(1e-35))`, and `python/gen_fixtures.py` probes values between the two.
- Calibration is `np.interp` semantics written step by step in both languages (`Calibrator.Apply`, `internal/model/calibrate.go:72`, and `riskgate.interpolate`). NumPy's own `np.interp` may fuse a multiply-add depending on how it was built. In Go the `float64(...)` conversion forbids fusion.

## 3. Why the model feeds the rules instead of deciding

- **Merchants tune rules, not weights.** A threshold on `:risk_score:` or an allow rule is something a fraud analyst can change and backtest. A model is not.
- **Merchants know things the model does not.** Trusted partners, expected spikes, products they never ship somewhere. Rules carry that.
- **Explainable decisions.** The log says which rule decided and why, which support can repeat to a customer.
- **Blast radius.** A bad rule is one line to revert, and shadow mode shows its live effect before it is enforced. A bad model affects everything.

This is also Radar's design, where the risk score is an attribute rules read. `risk_score` is a calibrated probability in percent, not a percentile (`model.RiskScore`, `internal/model/calibrate.go:105`). So `>= 50` means the same thing tomorrow even if traffic shifts, as long as the calibration holds. On the test month the isotonic fit cut expected calibration error from 0.0071 to 0.0038 (10 equal-width bins).

**The tuned rule set backs this up** (`rules/default.rules`, log in `results/rules_default/TUNING.md`). It is three live rules and two shadow rules, tuned on the validation month only.
- `block if :risk_score: >= 50`. Blocking pays when p > a / (2a + 15) with a $15 dispute fee, which is just under 50%, so 50 is the break-even and not a number I fit. Validation said 63 would have made $1,199 more, on one calibration step of 81 payments, and I did not tune to that.
- `review if :risk_score: >= 25`, the lowest score step with at least 25% fraud.
- `review if :product_code: = "C" and :amount: > 50 and :risk_score: >= 15`, which adds 43 frauds for 110 reviews.

Every starter rule and every rule from the hand-tuned baseline was dropped once the model was in, and each drop has its incremental backtest in the log. The whole set on validation flags 2,263 payments at 39.1% precision and 1.71% FPR. The old placeholder set flagged 8,978 at 8.8%. The honest summary is that hand-written rules add a little over this model on this month, and the log shows how little.

## 4. Radar's evaluation order and the missing-value semantics

**Order** (`RuleSet.Evaluate`, `internal/rules/ruleset.go:67`). Allow first, and an allowed payment skips block and review. Then block, and a blocked payment skips review. Then review. No match means allow. Radar puts request-3DS rules before all of these, and RiskGate leaves 3DS out. Within one action Radar leaves rules unordered. RiskGate reports the first match in source order so the logged rule is deterministic.

**Defending the order.** It makes rules composable. An allow rule wins over every block rule, present and future, without the author reading them, and adding a block rule can never un-block anything. The cost is that a careless allow rule lets fraud through, which is why the backtester reports overrides for allow rules (`Report.OverridesBlock`, `internal/backtest/report.go`). That report is why `default.rules` has no allow rule. The baseline's allow rule would have released 7 flagged payments, 4 of them fraud.

**Missing values** (`internal/rules/doc.go`). Kleene three-valued logic, and a rule matches only when its condition is TRUE, the same as a SQL `WHERE`.
1. A comparison, `in` or `starts_with` with a missing operand is UNKNOWN, including `!=`.
2. `not UNKNOWN` is UNKNOWN. FALSE decides an `and`, TRUE decides an `or`.
3. `is_missing(x)` is never UNKNOWN. It is how a rule asks for absent data.
4. Arithmetic with missing is missing, and so is division by zero.

The consequence is that `not :amount: > 100` and `:amount: <= 100` agree, and neither matches a missing amount.

**The story worth telling.** The first version was two-valued, and `not` flipped a missing comparison to true. Checking the docs against Radar found the mismatch. The fix switched both evaluators, the differential test showed they still agreed on every row, and a planted "two-valued `not`" bug was caught right away. Neither evaluator needs a third value at run time. Each node answers "is it TRUE?" or "is it FALSE?", `not` switches the question, and missing answers no to both.

## 5. What a Pratt parser does with `a or b and c`

Every operator has a binding power. Here, loosest first, that is `or`, `and`, `not`, comparisons and `in`, `+ -`, `* /`, unary minus (`internal/rules/parser.go:19`).

`parseExpr(minPrec)` (`internal/rules/parser.go:251`) parses an operand, then loops. While the next operator binds tighter than `minPrec`, it consumes the operator and parses the right side with `parseExpr(thatOperator'sPrec)`, so the right side only takes operators that bind tighter still.

Walk-through. The top call parses `a` and sees `or`, which is above the lowest level, so it parses the right side at `or`'s level. That call parses `b` and sees `and`, which binds tighter than `or`, so it keeps going and returns `b and c`. The result is `a or (b and c)`, same as `1 + 2 * 3`. Left associativity falls out because the loop continues at the same level.

Extras worth mentioning.
- Comparisons do not chain. The loop remembers it just parsed one and rejects `1 < :amount: < 5` with a fix.
- `x not in @list` is the one infix use of `not`, handled by peeking one token ahead.
- `TestPrecedence` (`internal/rules/parser_test.go:40`) pins the table, and `FuzzParse` checks `parse(print(ast)) == ast` with minimal parentheses. The fuzzer found two parser bugs, a quadratic checker and one diagnostic per junk byte (`internal/rules/BUGLOG.md`).
- Why Pratt over recursive descent with a function per level. It is the same thing with the levels in a table, so adding an operator is one table entry.

## 6. The three velocity-state designs, and which ships

| | Exact | Bucketed ring | Sketch |
|---|---|---|---|
| Structure | per-key deque of events, running totals | per-key, per-window ring of time buckets, only non-empty ones stored | count-min per time bucket, HyperLogLog for distinct cards, a table of 64-bit key hashes for first/last seen |
| Memory on IEEE-CIS, end of replay | 18.4 MB, 380 bytes per live key | 29.6 MB, 613 per key | 69.5 MB fixed |
| Error against exact | none, the reference | never under, over by at most one bucket | counts never under, collisions grow with total traffic |
| Validation ROC-AUC, model not retrained | 0.84505 | 0.84500 | 0.84267 |
| Code | `NewExact`, `internal/features/exact.go:13` | `NewBucketed`, `internal/features/bucketed.go:44` | `NewSketch`, `internal/features/sketch.go:125` |

Numbers from `results/state_shootout/README.md`. Concurrency is `NewSharded` (N shards, each an RWMutex padded to a 128-byte cache line), compared against one mutex and `sync.Map` (`internal/features/concurrent.go`). Hashing is FNV-1a with a finalizer, not `maphash`, because shard and sketch positions go into snapshots and must survive a restart.

**Which ships.** Exact, the service default. At this scale it is the smallest as well as the only one with no error, because most keys see a few payments a week and a short deque beats a ring sized per window. Idle-key eviction cut it from 74 MB to 18 MB with every feature bit-identical. The bucketed ring is the one I would switch to under a card-testing burst, since it bounds memory per key, and it costs nothing measurable on the metrics. The shard count is not settled, because experiment 5 ran on a loaded machine.

**The sketch bug, which is the best story here.** The first shootout run showed the sketch reporting never-seen keys as seen. First and last seen were min/max sketches shared by every key, and a key read as seen when all four of its cells held a time. Once other keys had touched every cell, every new card or customer looked old, with somebody else's first-seen time. That was 392,560 wrong values over the dataset, and `uid_seconds_since_first` was bit-exact on only 23.5% of rows. I found it from the per-feature error report (`features.CompareStates`), not from the AUC, which only dropped 0.0137. The fix replaced them with a fixed table of full 64-bit key hashes in 8-way sets (6.3 MB). Its only error is forgetting a key when a set overflows, which makes a key look newer, never older. That happened on 9 payments in six months, `uid_seconds_since_first` went to 99.996% bit-exact, and the sketch's ROC-AUC cost fell to 0.0024 (`internal/features/BUGLOG.md`).

## 7. Saabas contributions, and how they differ from SHAP

**Saabas** (`Model.Contributions`, `internal/model/saabas.go:32`). Follow the payment's path through each tree. At each step from a node to a child, credit the change in the node's expected value (LightGBM's `internal_value`) to the feature that node split on. A tree's credits telescope to leaf minus root, so bias plus contributions equals the raw score (`TestSaabasSumsToRawScore`). The service gets the score and the contributions from one walk of each tree (`Scorer.ScoreContributions`, `internal/model/scorer.go:130`), and the raw score keeps `PredictRaw`'s exact bits.

**TreeSHAP** (Lundberg, Erion and Lee, 2018) averages a feature's marginal contribution over all orders features could be revealed in. That gives consistency, meaning a model that relies more on a feature never gives it less credit, and fair treatment of interactions.

**The difference in one sentence.** Saabas credits only the one order the tree happens to test features in, so it is path-dependent and biased toward features split near the root, and the Lundberg paper names it as inconsistent.

**Why Saabas is fine here.** A reason string only asks which features pushed this score up. RiskGate never uses the contributions for global importance or anything that needs consistency. LightGBM's `pred_contrib` is TreeSHAP and will not match, and nothing in the repo calls these SHAP values.

## 8. Label delay, and why a naive backtest lies

Fraud labels come from disputes, and disputes arrive weeks after the payment, up to the networks' 120-day limit. A backtest over the last 30 days sees only the disputes that have already arrived. It **undercounts fraud**, so a block rule looks less precise than it is and an allow rule looks safer than it is. That is the dangerous direction.

**The fix.** Exclude payments younger than a maturity window and say so in the summary. The default is 60 days (`DefaultMaturity`, `internal/backtest/report.go:29`).

**The honest part.** IEEE-CIS has no label arrival times. Experiment 7 **simulates** them from an assumed lognormal delay, median 30 days, sigma 0.5, capped at 120 days (`DefaultDelay`, `internal/backtest/labeldelay.go:35`). The draw depends only on the seed and the payment id, so it is stable under filtering. On four rules from `rules/baseline.rules`, with "today" at the end of the validation month so no test row is used, the naive last-30-days backtest understated fraud dollars caught by 80% to 88% and made precision look 5 to 8 times worse. With 60 days of maturity the understatement was 4% to 6% (`results/label_delay/README.md`). All of that is SIMULATED, and the size depends on the assumption. With a 45-day median, 60 days of maturity still understates by about a sixth. The direction does not depend on it.

## 9. Fail open against fail closed

**Assess fails open.** If RiskGate is down or past the deadline, Clearinghouse lets the payment through as `risk_unavailable`. The reasoning is that a fraud check filters revenue, and failing closed turns a RiskGate outage into a total payment outage when most payments are legitimate.

**The cost.** Every payment in the outage is unchecked, and an attacker who can degrade RiskGate gets a window. Experiment 9 is meant to count it (payments unchecked, how many were fraud, Clearinghouse's p99 during the outage). It has not been run, and I say so rather than estimate.

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

## 11. What the data turned out to say

Three findings from tuning the rules on the validation month (`results/rules_baseline/TUNING.md`). Each one changed a rule, and each one is why the velocity gain is smaller than a textbook would predict.

- **`device_info` is a model string, not a device.** Values are things like "Windows" or a phone model, and 80% of payments have none. So `distinct_cards_per_device_24h > 5`, the textbook card-testing rule, blocked 7,613 validation payments at 6.8% fraud in the old starter set. It is gone from `default.rules`.
- **`card1` is shared.** Busy `card1` values belong to many customers, so a high card count is *less* fraudulent than average (0.5% fraud above the 99th percentile of `card_txn_count_24h`, against a 3.4% base rate).
- **`uid` is missing on product C.** Product C is 10% of payments and 38% of fraud, and almost none of it has `billing_region`, so the customer-level velocity features are missing exactly where fraud concentrates. That is also why the one hand-written rule that survived in `default.rules` is about product C.

The email entity is the purchaser email **domain**, because that is all the data has. So `distinct_cards_per_email_24h` on `gmail.com` mostly measures traffic.

## Hard questions

**Why is your AUC so far below the Kaggle winner?** The winner scored 0.9459 on Kaggle's hidden test set. I report 0.8105 on my own time split, so the two are not the same measurement. The bigger reason is a choice. The model sees only the eleven raw fields a rule author can name plus my velocity features, and it leaves out the hundreds of anonymous Vesta columns (`C1`-`C14`, `D`, `M`, `V`) that carry most of the signal in competition solutions. A rule cannot name them and a reason cannot explain them. I was not trying to win the leaderboard. The claim is the ablation, raw against raw plus velocity, on the same split (DESIGN.md, experiment 1).

**Why is the velocity gain modest?** Recall at 1% FPR went from 14.8% to 16.8%, and fraud-dollar recall from 15.6% to 20.5%, about a third more dollars. It is modest because the entities are weak on this data, as section 11 shows. Device is a model string, `card1` is shared, email is a domain, and the customer key is missing on the product where 38% of the fraud is. Velocity features are only as good as the key you count over. I would rather publish a real 2-point gain with the reason than a bigger number from a leaky split.

**What would you do with real identities?** Swap the entity keys, not the engine. A real card fingerprint, device id and customer id would go into `features.KeysOf` (`internal/features/key.go`) in place of `card1`, `device_info` and the reconstructed `uid`, and everything downstream (state, export, parity, backtests) would follow because it is one feature path. Then I would expect the dropped rules to come back. Distinct cards per real device is the card-testing signal, and it is useless here only because the device is not a device. I would also add IP and email address entities, and retrain, since the current model learned around the weak keys.

**Why fail open?** Section 9. A fraud check is a revenue filter, not a safety interlock, and most payments in any outage are legitimate. The cost is a window attackers can aim for, which is why experiment 9 exists to count it, and why webhook verification in the same service fails closed.

**How do you know the model in prod is the one you evaluated?** Two ways, and one gap. Experiment 2 proves the function is the same. All 590,540 payments went through the running service and every feature, encoded input, raw score, probability and `risk_score` matched the offline pipeline bit for bit, and the Go trees matched LightGBM on all 92,427 test rows. The provenance ties the files together. `results/exp2/exp2.md` records the sha256 of `model.txt` and `export.csv`, and `python/evaluate.py` refuses to score an export whose fingerprint differs from the one in the model's `metadata.json`. `riskgate audit` re-evaluates every logged decision against the rule-set version that made it. The gap is that the running service does not report a hash of the model it loaded, in `/v1/info` or in the decision log. That is the first thing I would add, so the check works on a live box and not only in an experiment.

**What did the shared signature vectors catch?** Seven disagreements between my Go verifier and Clearinghouse's Ruby verifier, before either shipped (DESIGN.md, "Signatures"). Uppercase hex in `v1` was accepted by Go and rejected by Ruby. A non-hex, short, long, or `=`-containing `v1` was a malformed header in Go and a non-matching signature in Ruby. A `t` with leading zeros was rejected by Go even though the MAC covers `t` as sent. A `t` with too many digits parsed in Go. Each was settled once, written into the grammar in `internal/webhook/signature.go`, and pinned by a vector both repos run, and in all seven Go moved to the behaviour closer to Stripe's. CI also checks my 73 vectors against a third implementation in Python and `openssl` (`scripts/check_signature_vectors.sh`), so the vectors are not only checked against the code that wrote them.

**Why three-valued logic?** Because most of IEEE-CIS is missing, so the rule for missing decides what most rules do. With two-valued logic `not :amount: > 100` matched every payment with no amount while `:amount: <= 100` matched none, and an analyst cannot see that difference by reading the rule. Kleene logic makes the two forms agree, matches Radar's documented behaviour for `NOT` over a missing attribute and SQL's `WHERE`, and keeps De Morgan. It gives up the excluded middle, and a rule that wants missing data says `is_missing(...)` explicitly. It costs nothing at run time, because each node only answers "is it true" or "is it false" (section 4, `internal/rules/doc.go`).
