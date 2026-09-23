# RiskGate design

RiskGate is a fraud rules engine written in Go. A payment service calls it before money moves, and it answers allow, block, or review, with a calibrated risk score, the rule that decided, and up to three reasons in plain English. Fraud analysts write the rules in a small typed language, and a backtester replays history to show what a rule would have done before it goes live. The model is one input to those rules, not the decision.

The rule semantics follow Stripe Radar's documented evaluation order on purpose. A fraud analyst who knows Radar should not have to relearn what `allow` means, and Radar's order is a reasonable design that I would rather copy and credit than reinvent. RiskGate is not a Radar clone. It has no network data, no card country, and one merchant's worth of history, and nothing in this repo claims it performs like Radar.

The single property everything below serves is that **a rule does online exactly what its backtest said it would do**. That takes one feature implementation, two rule evaluators that provably agree, and a model evaluator that matches the trained model bit for bit. Most of this document is about how each of those is made true and how it is checked.

Numbers written as `TBD (experiment N)` have not been measured yet. Nothing in this document is an estimate standing in for a measurement.

## Contents

1. [What the data can and cannot say](#what-the-data-can-and-cannot-say)
2. [Data license](#data-license)
3. [Architecture](#architecture)
4. [One feature implementation, and point-in-time correctness](#one-feature-implementation-and-point-in-time-correctness)
5. [Velocity state](#velocity-state)
6. [The model](#the-model)
7. [The rule language](#the-rule-language)
8. [Two evaluators and the differential test](#two-evaluators-and-the-differential-test)
9. [The backtester](#the-backtester)
10. [The online service](#the-online-service)
11. [Integration with Clearinghouse](#integration-with-clearinghouse)
12. [Experiments](#experiments)
13. [Bug log](#bug-log)
14. [What I would do next](#what-i-would-do-next)
15. [Reading list and credits](#reading-list-and-credits)

## What the data can and cannot say

RiskGate runs on the IEEE-CIS Fraud Detection dataset, real e-commerce transactions from Vesta Corporation released through a 2019 Kaggle competition sponsored by the IEEE Computational Intelligence Society. The labelled training file has 590,540 transactions, about 3.5% of them fraud. That is real data, and it is also data with hard limits. They shape every feature and every rule, so they come first.

- **Most fields are anonymized.** Card, address and distance fields are opaque codes. There is no card country and no IP country. Rules can only use fields that exist, so the field catalog (`internal/schema/catalog.go`) is built from the real columns. `amount`, `product_code`, `card_network` (card4), `card_type` (card6), `purchaser_email_domain` (P_emaildomain), `recipient_email_domain` (R_emaildomain), `device_type`, `device_info`, `distance` (dist1), `billing_region` (addr1) and `billing_country_code` (addr2) are the raw fields, plus RiskGate's velocity features and `risk_score`. A rule such as `block if :purchaser_email_domain: = "anonymous.com" and :amount: > 300` is real. A rule about card country is not, and RiskGate does not pretend it is.
- **Customer identity is approximated.** The dataset has no customer id. The competition's first-place team (Chris Deotte and Konstantin Yakovlev) rebuilt one from `card1`, `addr1`, and the day the card was first used, recovered as the transaction day minus `D1`. They used it to group transactions for aggregate features and not as a feature itself. RiskGate does the same thing. It uses that `uid` as one entity key for velocity features, never as a model input, and credits the team for it (`features.UIDKey`, `internal/features/key.go:87`). It is an approximation. Two customers can share a `uid`, and one customer splits across several when `D1` is missing, clipped, or drifts. Reasons that mention it say "this customer", and this document calls it an approximation every time.
- **Email is domain only.** The dataset has `gmail.com`, never an address. So the `email` entity and `distinct_cards_per_email_24h` are domain-level signals. "Payments from anonymous.com in the last hour" is meaningful. "Payments from this person's email" is not available, and on a large domain the distinct-card count mostly measures traffic.
- **`card1` is coarse.** Many customers share a `card1` value, which is why `uid` exists at all.
- **Labels are mature, and have no arrival time.** `isFraud` comes from chargebacks and was assigned after the fact. A real system learns about fraud weeks later. The data does not say when each label arrived, so experiment 7 simulates arrival times from a stated assumption and labels every number it produces as simulated.
- **Time is an offset.** `TransactionDT` counts seconds from an undisclosed reference. Where a calendar date is printed, RiskGate assumes the reference is 2017-12-01 00:00 UTC, the date commonly assumed in the competition's discussion (`backtest.DefaultEpoch`, `data.ReplayEpochUnix`). Nothing depends on that date being right, only on it being fixed.
- **Evaluation uses a time split.** The Kaggle test file has no labels, so all evaluation splits the labelled file by time. `TransactionDT` spans about 182 days. `internal/data/split.go` cuts it into 30-day months aligned to midnight. Months 0 to 3 train, month 4 is validation, month 5 is test, and the partial seventh month (about two days) is folded into test rather than dropped. Random splits would put a customer's later payments in training and their earlier ones in test, which is leakage by another name.

## Data license

The IEEE-CIS data is covered by the competition's rules on Kaggle. Section 7 of the general competition rules, "Competition Data", allows use "for non-commercial purposes only", including academic research and education, and asks participants not to redistribute the data to anyone who has not accepted the rules. This project is non-commercial and educational, so using the data is fine. Redistributing it is not, and that shapes what this repository contains.

- **Raw data is never committed.** `scripts/fetch_data.sh` downloads it with your own Kaggle credentials after you accept the rules yourself. `data/` is in `.gitignore`.
- **Row-level derived data is never committed or published either.** That covers the binary cache (`data/cache/*.rgc`), the feature export (`features.csv`), the Clearinghouse replay file (`test_replay.jsonl`), decision logs, and the sample payments a backtest report shows. A feature table is the dataset with extra columns, and a replay file is the dataset in JSON, so the same rule applies to them.
- **Only aggregate results are published.** AUCs, counts, dollar totals, latency percentiles, and plots. A number in this document or the README never identifies a transaction.
- **Tests run on synthetic data.** `cmd/synth` writes an IEEE-CIS-shaped dataset from a seeded generator, and every tool that reads it prints `SYNTHETIC` (`data.SyntheticMarker`). The model parity fixtures under `internal/model/testdata` are small models trained on generated inputs.
- **The page and the API show sample payments only locally.** A backtest report includes up to ten matching payments so an analyst can see what a rule catches. That is fine on your machine against data you downloaded. It is not fine in a published screenshot, so the demo recording uses synthetic data.

## Architecture

```
            Clearinghouse (Ruby) confirm path                Rule author (browser)
                     |  POST /v1/assess                              |
                     |  (deadline set by Clearinghouse)              |
                     v                                               v
  +-------------------------------------------+      +---------------------------+
  |  RiskGate service (Go)          [planned] |      |  Backtest page and API    |
  |                                           |      |  [backtester built,       |
  |  features.Engine ---- velocity state      |      |   page planned]           |
  |        |              exact | bucketed |  |      |                           |
  |        |              sketch, sharded     |      |  columnar table (.rgt)    |
  |        v                                  |      |  vectorized rule eval     |
  |  model.Scorer (LightGBM trees in Go,      |      |  labels + maturity        |
  |        |       isotonic calibration,      |      +---------------------------+
  |        |       Saabas reasons)            |                   ^
  |        v                                  |                   |
  |  rules.RuleSet (closures, atomic swap)    |      same features.Engine, same
  |        v                                  |      rule checker, replayed over
  |  decision + reasons -> decision log       |      history (features.Replay)
  +-------------------------------------------+
                     ^
                     |  webhooks from Clearinghouse: payment_intent.*, charge.dispute.*
                     |  (internal/webhook: signature verified, deduped by event id)
                     |  -> labels
```

What exists today, package by package.

| Package or command | What it does | State |
|---|---|---|
| `internal/schema` | The field catalog, the one contract between features, rules, model and backtester | built |
| `internal/data` | IEEE-CIS loader, identity join, `(DT, ID)` sort, month split, binary cache, replay format | built |
| `internal/data/synth`, `cmd/synth` | Seeded synthetic dataset in the IEEE-CIS file format | built |
| `internal/features` | The feature engine, three velocity states, three concurrency wrappers, snapshots, replay | being finished |
| `cmd/export` | Offline feature export for training, plus the test-month replay file | built |
| `internal/model` | LightGBM text-model evaluator, Saabas contributions, calibration, reasons | built |
| `python/` | Offline training, baselines, evaluation, parity scores, fixtures | built |
| `cmd/parity` | Go raw scores against LightGBM's on every exported row | built |
| `internal/rules`, `cmd/rulecheck` | Lexer, Pratt parser, checker, linter, printer, closure compiler, generator | built |
| `internal/backtest` | Columnar table, vectorized evaluator, differential test, reports, sweep, label delay | built |
| `cmd/backtest` | Command line for backtests, the sweep, and experiments 3, 6 and 7. Building its table from the real export is not wired up yet | built |
| `internal/webhook` | Clearinghouse signature verifier, dedupe, event envelope, HTTP handler | built |
| `internal/loadgen`, `cmd/loadgen` | Open-loop load generator with coordinated-omission correction | built |
| `internal/service`, `cmd/riskgate` | `/v1/assess`, idempotency, rule swap, decision log, snapshots, metrics, the page | in progress (idempotency and metrics started, no binary yet) |

## One feature implementation, and point-in-time correctness

Train/serve skew is the most common way a fraud model fails quietly. The model is trained on features computed one way, usually a batch job in Python or SQL, and served on features computed another way, usually a streaming service written later by someone else. The two disagree at a window edge, on a null, or on whether "the last hour" includes the current payment. Nothing crashes. The model just gets worse, and nobody can say by how much.

RiskGate rules this out by construction. There is one function that turns a payment and the current state into a feature vector, `Engine.ScoreAndUpdate` (`internal/features/engine.go:171`). The offline export calls it through `features.Replay`. The backtester's table is meant to be built from the same replay (`features.ReplayColumns` produces exactly the column layout it scans), and today `cmd/backtest` runs only on generated tables until that step is wired up. The online service will call it once per request. Python never computes a feature. It reads the Go export (`python/riskgate.py`), and it even parses floats with `float_precision="round_trip"` because pandas' default parser can be one ulp off, which would break parity before the model is involved.

Construction is a claim. Experiment 2 is the proof. It replays the test month through the HTTP service, captures every feature vector, and compares it with the offline export row by row.

### Point in time

A feature for a payment at time *t* may use only payments strictly before it. Three things enforce that.

1. **Score before update.** `ScoreAndUpdate` reads the state, writes the row, and only then adds the payment. A payment never sees itself. Each key is read and updated under one lock (`State.ReadAdd`), so two concurrent payments on the same key each see the other entirely or not at all.
2. **A total order.** Replay feeds payments in `(TransactionDT, TransactionID)` order (`data.Less`, `internal/data/txn.go:30`). IEEE-CIS has many payments in the same second, and "strictly before" is ambiguous for them unless the order is total. The tie-break by `TransactionID` makes it total and deterministic. A payment at the same second with a larger id counts as later. `Replay` checks the order as it goes and fails on a violation, because an out-of-order input would leak the future silently.
3. **Windows exclude the present.** Every count and sum covers strictly earlier payments of the same entity inside the trailing window. The schema's field docs say so, and `TestWindowEdges` pins the boundary to the second.

### The leakage test

`TestLeakage` (`internal/features/leakage_test.go:35`) checks the property directly instead of trusting the three mechanisms above. For each sampled payment *i* it rewrites history after *i*. It mutates or deletes every later event and inserts new ones, including payments at the same second with a larger `TransactionID` and payments on the same card and device. Then it shuffles the whole input, runs the same pipeline the export runs (`data.Sort`, then `Replay`), and requires the features of *i*, and of everything before it, to be bit-identical to the untouched run. It runs against all three state implementations. CI runs a small sample under the race detector, and one environment variable runs it over thousands of payments on a full dataset.

### Event time, not processing time

Windows are defined in event time, the payment's own `created` timestamp, never the wall clock of the machine computing the feature. Replay has no wall clock at all. Online, Clearinghouse sends `created`, and the service converts it back to `TransactionDT` (`data.DTFromUnix`), so a replayed month computes the same windows at any replay speed. Kleppmann's chapter on stream processing is the reference here.

The cost of event time is out-of-order arrival. Online, two requests for the same card can arrive in the opposite order from their timestamps. RiskGate's rule is that a late event is recorded at the key's latest time rather than its own (`TestLateEventIsClamped`). It is still counted, and a key's history stays in time order, so windows never have to be repaired. The trade-off is that a late event can stay in a window for up to a few seconds longer than it should. I chose that over buffering, because a buffer would add latency to every payment to fix a rare edge.

One more honest limit. Per-key atomicity is all the engine promises. Two concurrent payments that share a card but not a device can see each other through the card and not through the device. Replay is sequential and never hits this. Online it means features depend on arrival order within a few milliseconds, which is also true of any real system that does not serialize all payments.

## Velocity state

For each entity (card, `uid`, device, email domain) and each window (1 hour, 24 hours, 7 days), RiskGate keeps the payment count and dollar sum. From those it derives the 7-day mean, the ratio of this amount to that mean, seconds since the entity was first seen and since its previous payment, and distinct cards per device and per email domain in 24 hours, which is the card-testing signal. The field list is generated in `schema.VelocityFieldNames`.

The state has to be bounded in memory, fast under concurrency, and correct at window edges. There are three implementations behind one `State` interface, and experiment 4 measures them against each other.

| State | How | Memory | Error |
|---|---|---|---|
| **Exact** (`NewExact`) | A per-key deque of every event in the last 7 days, with running totals per window. Reads walk only the events that expired since the last add. | Grows with traffic. A card-testing burst on one device grows that device's deque for a week. | None. It is the reference every other state is measured against, and `TestExactMatchesOracle` checks it against a brute-force oracle. |
| **Bucketed ring** (`NewBucketed`) | Per key and window, a ring of fixed time buckets (1-minute for the hour, 15-minute for the day, hourly for the week), storing only non-empty buckets. Distinct cards stop growing at a cap of 1,024. | Bounded per key by the bucket count, however many payments the key makes. | Only at the trailing edge. Counts and sums never undercount and overcount by at most one bucket's contents. A saturated distinct count reports the cap. |
| **Sketch** (`NewSketch`) | Count-min sketches per time bucket for counts and sums, HyperLogLog registers for distinct cards, min and max sketches for first and last seen. Shared by all keys. | Fixed at construction, independent of the number of keys. | Counts never undercount (`TestSketchNeverUndercounts`). Collisions push estimates up by an amount that grows with total traffic in the window, not the key's own. |

The sketch's error is reported, not hidden. `features.CompareStates` replays history through the exact state and an approximate one in lockstep and reports each feature's error. More important than per-feature error is what the approximation does to the thing people care about, fraud caught at a fixed false-positive rate. Experiment 4 retrains on each state's export and reports that. If the sketch costs nothing measurable, that is a finding. If it does, that is also a finding.

Idle keys are evicted after 30 days (`DefaultIdleTTL`). Eviction only frees memory. An idle key already reads as unseen, so results do not depend on when a sweep runs.

For concurrency, `NewSharded` hashes keys across N states, each behind its own read-write mutex and padded to Apple silicon's 128-byte cache line. `NewLocked` (one mutex) and `NewSyncMap` (per-key entries in a `sync.Map`) are the baselines. The key hash is FNV-1a with a finalizer, deliberately not `hash/maphash`, because shard assignments and sketch cells are written into snapshots and must mean the same thing after a restart. Experiment 5 sweeps N.

Snapshots are a small binary format, deterministic in order, so equal states produce byte-identical snapshots. That makes "the restored state equals the state before shutdown" a byte comparison (`TestSnapshotRestore`).

Which state ships is decided by experiments 4 and 5. `TBD (experiments 4 and 5)`.

## The model

### Why the model feeds rules instead of deciding

The model produces `:risk_score:`, and rules read it like any other attribute. `block if :risk_score: >= 85` is a rule, and so is `allow if :purchaser_email_domain: in @trusted_domains`, which can override the model. This is Radar's design, and it is the right one for three reasons.

1. **Merchants tune rules, not weights.** A merchant who is losing good customers can raise a threshold or add an allow rule and see the effect in a backtest. Nobody can do that with a gradient-boosted model.
2. **Merchants know things the model does not.** A merchant's own trusted-customer list, a promotion that makes a spike expected, a product that is never shipped to certain regions. Rules carry that knowledge.
3. **The decision is explainable in one line.** The decision log says which rule decided. "Blocked by `block if :risk_score: >= 85`, score 91, because 9 payments on this card in the last hour" is something support can say to a customer.

### Training

Training is offline, in Python, on the Go export (`python/train.py`). It never touches the test month. The script drops test rows as the CSV is read, so they never reach its memory. Everything is chosen on the validation month, which is LightGBM's iteration count (early stopping), a small hyperparameter grid, logistic regression's `C`, the calibration, and the operating thresholds. Selection is by validation PR-AUC, because at a 3.5% base rate ROC-AUC flatters every model.

Four baselines run on the same split.

| Baseline | What it is |
|---|---|
| Rules alone | A hand-written rule set, scored by RiskGate's Go rule engine and passed to Python as predictions. Python does not reimplement rules any more than it reimplements features. |
| Logistic regression | All features, with signed `log1p`, median imputation plus missing indicators, and one-hot categoricals. |
| LightGBM, raw columns | Only the fields taken from the transaction itself (`schema.RawFields`). |
| LightGBM, raw plus velocity | Raw fields plus RiskGate's velocity features. This is the model the service runs. |

The gap between the last two is what the streaming features are worth. That ablation is experiment 1 and is the claim of the project. The test month is scored once, at the end, by `python/evaluate.py`, and that number is published whatever it is.

### Calibration and what `risk_score` means

An isotonic regression is fitted on the validation month, mapping the raw score to a probability. `risk_score` is that probability times 100, floored and clamped to 0 through 99 (`model.RiskScore`, `internal/model/calibrate.go:105`). **It is a calibrated probability in percent, not a percentile.** A `risk_score` of 20 means about a one-in-five chance of fraud as calibrated on the validation month, however many payments score above or below it. A percentile would move every time traffic changed, and a threshold rule on it would silently change meaning. A calibrated probability keeps `block if :risk_score: >= 85` meaning the same thing from one day to the next, as long as the calibration holds, and experiment 1 reports whether it holds on the test month with a reliability plot, Brier score and expected calibration error.

The isotonic fit is on the raw score rather than on `sigmoid(raw)`. Isotonic regression is invariant to monotone transforms of its input, so the fit is identical, and this keeps `exp` out of the path. `math.Exp` and NumPy's `exp` are not guaranteed to agree to the last bit.

### The Go evaluator

`internal/model` reads LightGBM's text model file and predicts raw scores in Go with no cgo, no ONNX runtime, and no Python. It handles numerical splits, missing values from the `decision_type` bits (default left or right, and whether NaN or zero counts as missing), and categorical splits from the `cat_threshold` bitsets. Trees are summed in file order into a float64 starting at zero, which is what LightGBM's own `PredictRaw` does. Summation order is the usual reason two tree evaluators differ in the last bit, so matching it is what makes bit-identical parity possible.

**Parity.** The target is bit-identical raw scores, not "close". The unit tests compare against LightGBM's `predict(raw_score=True)` on fixtures built to break an evaluator that is merely close (`python/gen_fixtures.py`). Rows are fed at every split threshold, one ulp above and below it, at both signed zeros, at values inside LightGBM's zero threshold, and at NaN and infinities. Categorical fixtures probe codes past the bitset, negative codes and fractional codes. On real data, `cmd/parity` compares every row of the test month against scores written by `python/parity.py` and exits 1 on any difference. Result `TBD (experiment 2)`.

Two findings from building it are worth recording, because both are the kind of thing that makes an evaluator agree on 99.99% of rows.

- **LightGBM's zero threshold is a float literal.** `kZeroThreshold` in LightGBM's `meta.h` is `1e-35f`. Its double value is `float32(1e-35)`, about `1.0000000180025095e-35`, not `1e-35`. An evaluator that writes `1e-35` sends a value between the two down the wrong branch of a zero-as-missing split. `internal/model/lightgbm.go:39` spells it `float64(float32(1e-35))`, and the fixtures probe exactly those values.
- **`np.interp` is not reproducible to the last bit.** NumPy's C loop computes `slope*(x-xp[j]) + fp[j]`, and whether the compiler fuses that into one multiply-add depends on how NumPy was built. The arm64 macOS wheel fuses, a baseline x86-64 build cannot. So the calibration step is written operation for operation in both languages. In Go the `float64(...)` conversion forbids fusion (the Go spec allows fusing only when no explicit conversion intervenes). In Python each step is its own NumPy call (`riskgate.interpolate`). `TestCalibratorMatchesPython` checks the two bit for bit.

### Reasons, and why they are Saabas and not SHAP

Each decision carries up to three reasons, such as "9 payments on this card in the last hour, usually fewer than 2". They come from per-feature contributions computed with the **Saabas** method, from Ando Saabas's `treeinterpreter`. Follow the path the payment takes through each tree. Every time the path moves from a node to a child, credit the change in the node's expected value to the feature that node split on. A tree's contributions telescope to its leaf value minus its root value, so the bias plus all contributions equals the raw score (`TestSaabasSumsToRawScore`).

This is not SHAP, and the difference matters. TreeSHAP (Lundberg, Erion and Lee) averages a feature's marginal contribution over every order in which features could be revealed. That makes it consistent, meaning a model that relies more on a feature never gives it less credit, and fair to interacting features. Saabas credits only the one order the tree happens to test features in, so it is path-dependent and biased toward features split near the root. Lundberg et al. use Saabas as their example of an inconsistent method.

RiskGate uses Saabas anyway because it costs about one prediction, needs no allocation, and answers the only question a reason string asks, which features pushed this payment's score up. It is not used for anything that needs consistency, such as global feature importance. LightGBM's `pred_contrib` output is TreeSHAP and will not match these numbers, and nothing in RiskGate calls them SHAP values.

## The rule language

```
allow  if :purchaser_email_domain: in @trusted_domains
block  if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:
block  if :risk_score: >= 85
review if :distinct_cards_per_device_24h: > 3
review if :product_code: = "C" and is_missing(:device_info:)
shadow block if :risk_score: >= 80
```

Attributes are `:name:` from the catalog, named lists are `@name`, text is double-quoted. Conditions combine with `and`, `or`, `not` and parentheses. Values compare with `= != < <= > >=`, test membership with `in` or `not in` against a named or literal list, and combine with `+ - * /`. The functions are `is_missing`, `lower` and `starts_with`. `#` starts a comment.

### Grammar sketch

```
ruleset  = { line } ;
line     = [ rule ] [ "#" comment ] newline ;
rule     = [ "shadow" ] action "if" expr ;
action   = "allow" | "block" | "review" ;
expr     = expr "or" expr | expr "and" expr | "not" expr
         | sum cmp sum | sum [ "not" ] "in" list | sum ;
cmp      = "=" | "!=" | "<" | "<=" | ">" | ">=" ;
sum      = sum ("+" | "-") term | term ;
term     = term ("*" | "/") unary | unary ;
unary    = "-" unary | primary ;
primary  = attr | number | string | "true" | "false"
         | func "(" [ expr { "," expr } ] ")" | "(" expr ")" ;
list     = "@" name | "[" [ literal { "," literal } ] "]" ;
```

The grammar above is ambiguous on purpose. Precedence resolves it, and the parser is where that happens.

### Pratt parsing, and `a or b and c`

The parser (`internal/rules/parser.go`) is a Pratt parser, from Vaughan Pratt's 1973 "Top Down Operator Precedence". Each operator has a binding power. Loosest first, they are `or`, `and`, `not`, the comparisons and `in`, `+ -`, `* /`, and unary minus.

The core is one loop, `parseExpr(minPrec)`. It parses an operand, then looks at the next operator. If that operator binds tighter than `minPrec`, it consumes it and parses the right-hand side with `parseExpr(thatOperatorsPrec)`, so the right side swallows only operators that bind tighter still. Otherwise it returns what it has to its caller.

For `a or b and c`, the top call parses `a` and sees `or`, which binds tighter than the lowest level, so it parses the right side with `parseExpr(prec(or))`. That call parses `b` and sees `and`, which binds tighter than `or`, so it takes `and c` itself and returns `b and c`. The result is `a or (b and c)`, exactly as `1 + 2 * 3` is `1 + (2 * 3)`. Operators at the same level are left-associative because the loop continues at the same level after each one. Comparisons get one more check. The loop remembers that it just parsed a comparison and rejects `1 < :amount: < 5` with a message saying to join the comparisons with `and`, because a chained comparison in this language would compare a boolean with a number.

`TestPrecedence` pins the table, and the fuzzer checks `parse(print(ast)) == ast` with minimal parentheses, which would fail if the printer and the parser ever disagreed on precedence.

### Types and missing values

The types are number, string and bool, and **missing** is a value of either number or string. Much of IEEE-CIS is null (only about a quarter of payments have an identity row, so `device_info` is usually missing), so this decision affects most rules. The checker rejects type errors before anything runs.

The rules, from `internal/rules/doc.go`, are these.

1. A comparison, `in` or `starts_with` with a missing operand is **unknown**. That includes `!=`: `missing != 5` is unknown, so it never matches.
2. `is_missing(x)` is true when `x` is missing and false otherwise, never unknown. It is how a rule says what it wants done with absent data.
3. `not`, `and` and `or` follow Kleene's three-valued logic. `not unknown` is unknown, `false and anything` is false, `true or anything` is true, and every other combination with unknown is unknown.
4. Arithmetic with a missing operand is missing, and so is division by zero. `lower(missing)` is missing.
5. A rule matches only when its condition is **true**. That is exactly how SQL evaluates a `WHERE` clause.

| `:amount:` | `:amount: > 100` | `not :amount: > 100` | `:amount: <= 100` | `is_missing(:amount:)` |
|---|---|---|---|---|
| 150 | true | false | false | false |
| 50 | false | true | true | false |
| missing | unknown | unknown | unknown | true |

So `not (x > 100)` and `x <= 100` agree on every row, and so do `not (x = "a")` and `x != "a"`. A comparison never matches absent data, whichever way it is written. A rule that wants absent data says so: `not :amount: > 100 or is_missing(:amount:)`.

**Why this, and how it got here.** The first version used two-valued logic, where a comparison with a missing value is false and `not` flips it to true. It is simpler, and it keeps every Boolean law. Checking it against Stripe's rules reference showed that it disagreed with Radar in exactly one place. Stripe's "Missing attributes" section says that `NOT` over a comparison with a missing feature "always returns false". Under two-valued logic `not :amount: > 100` matched every payment with no amount while `:amount: <= 100` matched none. An analyst reading the rule cannot see that difference, and a rule written by someone who learned Radar would block customers they never meant to block. Three-valued logic keeps the negated and the rewritten forms equal, matches Radar and SQL, and still obeys De Morgan's laws. What it gives up is the law of the excluded middle: `x > 100 or not x > 100` does not match a missing `x`. The switch was checked the same way as everything else. The reference interpreter was rewritten with an explicit third value, the two evaluators agreed on every row of the differential test, and a planted "two-valued `not`" bug was caught by both evaluators' tests and by the fuzzer within a second.

**No third value at run time.** Both evaluators compile each condition node to answer one question, "is it true?" or "is it false?", and `not` switches the question. A missing operand answers no to both. That is Kleene logic exactly, and it costs one closure per node online and one bitmap per node in the vectorized evaluator, the same as two-valued logic did.

### Evaluation order

`RuleSet.Evaluate` (`internal/rules/ruleset.go:67`) follows the order Stripe documents for Radar. **Allow** rules run first, and a payment an allow rule matches is not evaluated against block or review rules. Then **block**, and a blocked payment is not evaluated against review rules. Then **review**. A payment no rule matches is allowed. Radar also has request-3DS rules, which run before allow. RiskGate leaves them out, because real 3D Secure needs a card network (see [what I would do next](#what-i-would-do-next)). Radar has one more wrinkle RiskGate does not need. Rules that read post-authorization attributes, such as the CVC check result, run after rules that do not. RiskGate sees a payment once, before authorization, so it has no such attributes.

Why follow this order rather than a first-match-wins list? Because it makes rules composable. An analyst adding an allow rule for a trusted partner knows it wins over every block rule, present and future, without reading them. Adding a block rule can never un-block anything. The price is that an allow rule is powerful, and a careless one lets fraud through. The backtester's overrides count exists to show exactly that price before the rule goes live.

Radar leaves rules within one action unordered, since any match yields the same outcome. RiskGate evaluates them in source order and reports the first that matched, so the reported rule is deterministic, which matters for the decision log and for experiment 2.

### Shadow rules

`shadow block if :risk_score: >= 80` is evaluated on every payment and logged, and never decides. It is how a rule goes live safely, and it is the same idea as Radar's gradual rollout, where a rule at 0% of traffic runs in what Stripe calls shadow mode. The backtester says what it would have done on history, shadow mode says what it does on live traffic, and the two can be compared before the rule is enforced. A gap between them is a train/serve bug or a shift in traffic, and either is worth knowing before the rule blocks real customers.

### Errors are the product

A rule author is a fraud analyst, not a Go programmer. Every error names the position with carets, says what was wrong in the analyst's terms, and suggests a fix. Each message has a golden-file test under `internal/rules/testdata/errors/`. This is a real rendered example (`multiple_errors.golden`).

```
error at line 4, column 10:
block if :card_txn_cnt_1h: >= 8
         ^^^^^^^^^^^^^^^^^
unknown attribute :card_txn_cnt_1h:. Did you mean :card_txn_count_1h:?

error at line 5, column 22:
review if :amount: > "300" and :devise_type: = "mobile"
                     ^^^^^
:amount: is a number, and "300" is a string. Remove the quotes.

error at line 5, column 32:
review if :amount: > "300" and :devise_type: = "mobile"
                               ^^^^^^^^^^^^^
unknown attribute :devise_type:. Did you mean :device_type:?

error at line 7, column 1:
blok if :amount: > 5000
^^^^
unknown action blok. Did you mean block?
```

Every error in a rule set is reported at once, not the first one only. The linter adds warnings for rules that cannot match (`:amount: > 500 and :amount: < 100`), rules that match everything (`block if 1 < 2`, "this rule will block every payment"), and duplicate rules. A rule that is fully covered by rules already in force is found by the backtester, which has the data to know. There are 35 golden files, including tabs and Unicode in the source line, so the carets line up with what the analyst sees.

## Two evaluators and the differential test

There are two rule back ends, because online and offline want different shapes.

1. **Closures, online** (`internal/rules/compile.go`). A checked rule compiles into a tree of Go closures over one payment's feature vector, with constant folding and fast paths for an attribute against a constant. It evaluates with zero allocations (`TestZeroAllocs`).
2. **Vectorized, for backtests** (`internal/backtest/vector.go`). A checked rule compiles into a program over columns of the whole history. Numbers are float64 arrays with NaN for missing, strings are dictionary codes, and the result is a bitmap of matching rows. It works in 4,096-row chunks, decides string predicates once per distinct value through a lookup table, and refines an `and` only on the rows its left side left standing when those are few, like a selection vector in a column store. A 50-rule set is evaluated chunk-major, so each column chunk is read from cache once instead of streamed from memory 50 times.

This is the same lesson as Basalt, my vectorized query engine, applied to a different problem. Row-at-a-time evaluation pays interpretation overhead on every row, and column-at-a-time pays it once per chunk. Experiment 6 measures the gap here.

Two evaluators are a liability unless they are proven to agree, because a backtest from one and a decision from the other is exactly the gap this project exists to close. So they are checked against each other.

- **Generated rules.** `rules.Generate` produces random well-typed rules with nesting, arithmetic, missing values, both list kinds and every function. `backtest.Ground` replaces their constants with values drawn from the actual table, so comparisons actually split the data instead of being trivially false.
- **Adversarial rows.** The synthetic table injects edge values into every column. For numbers that means missing, both signed zeros, subnormals, infinities, the largest floats, and integers past 2^53. For text it means strings whose lowercase changes byte length (the Kelvin sign, a dotted capital I), Greek final sigma, invalid UTF-8, and case variants of the same domain.
- **The check.** `backtest.DiffTest` (`internal/backtest/diff.go:83`) evaluates every generated rule on every row both ways and reports any disagreement with the rule and row. The rule-set level is checked too. `EvaluateRuleSet` over bitmaps must equal `RuleSet.Evaluate` row by row, including which rule is reported.
- **Against a reference.** Inside the rules package, compiled closures are checked against a direct tree-walking interpreter on 3,000 generated rules times 200 generated rows on every `go test`, so the optimizations in the closure compiler are also checked.
- **Parser fuzzing.** `FuzzParse` feeds arbitrary text. It checks no panics, that every rule that parses prints to text that parses back to an equal tree, that printing is a fixed point, and that every rule that loads evaluates the same as the reference interpreter.

Rules generated, rows checked and disagreements found are `TBD (experiment 3)`. Bugs the fuzzer found are in the [bug log](#bug-log).

## The backtester

Given a proposed rule and the rule set in force, the backtester replays history and reports what would have changed (`backtest.Backtester.Run`, `internal/backtest/report.go:277`).

- **Incremental effect, not raw matches.** The rule set in force is evaluated once over the table. The report's headline is `Changed`, the payments whose decision this rule changes. A new block rule that only matches payments already blocked changes nothing, and the report says the rule is unreachable and which rule covers it. Raw matches are reported separately.
- **Outcomes by label.** Changed payments split into fraud and legitimate, by count and by dollars. Precision, and the share of all fraud dollars in the period that the rule catches.
- **Overrides for allow rules.** Following Stripe's backtest, an allow rule's report shows the payments it would let through that the current rules block or send to review, and how many of those were fraud. That is the real cost of an allow rule. Stripe's test summary groups payments into disputes and early fraud warnings, refunds, blocked and failed, and succeeded. IEEE-CIS has fraud labels but no refunds or early fraud warnings, so RiskGate's report has fraud and legitimate only.
- **Overlap.** How the rule's matches intersect each rule already in force, so an analyst sees whether the rule is new coverage or a copy of an old rule.
- **Plain English.** `Report.Summary` writes a paragraph in the merchant's voice, with counts, dollars and shares formatted so a small share never reads "0%" and a share short of all never reads "100%".
- **Samples.** Up to ten changed payments, spread across the period rather than the first ten, so an analyst sees the rule's typical catch.

### Label maturity

A backtest over the last week undercounts fraud, because disputes have not arrived yet. A naive backtest therefore makes every block rule look worse than it is and every allow rule look safer than it is. The backtester excludes payments younger than a maturity window by default and says how many it excluded, in the summary.

The default is 60 days (`DefaultMaturity`). IEEE-CIS carries no label arrival times, so this cannot be measured from the data. It is chosen from one stated assumption, the delay model experiment 7 uses. That model is lognormal with a 30-day median and sigma 0.5, capped at the networks' 120-day dispute limit, and under it about 92% of disputes arrive within 60 days. No window shorter than 120 days is complete, and 60 leaves four months of the 182-day dataset to backtest over. The trade is stated, and `Options.Maturity` changes it.

### Threshold sweep

"Which `risk_score` should I block at?" is the first question every merchant asks. `Backtester.Sweep` answers it for every integer threshold 0 through 99 in one pass. Since `score >= k` holds exactly when `floor(score) >= k`, payments go into 100 buckets and each threshold's answer is a suffix sum. For each threshold it reports precision, fraud-dollar recall, fraud-count recall and legitimate dollars blocked. Unscored payments are counted separately, because a missing score matches no threshold.

## The online service

This section describes the planned `internal/service` and `cmd/riskgate`. The pieces it assembles exist and are tested. The service itself is not built yet.

### `POST /v1/assess`

1. Decode the payment. Unknown `risk_fields` keys and wrongly typed values are errors, not ignored (`data.ParseRiskFields`). A typo such as `card_1` would otherwise become a missing value online while the export had the real one, which is train/serve skew arriving through the API.
2. Honor the caller's deadline from `RiskGate-Deadline-Ms`. If the budget is already spent, answer immediately rather than do work nobody will wait for.
3. Deduplicate by `Idempotency-Key`. Clearinghouse retries on timeout, and a retry must not count the payment twice in the velocity state. The same key gets the same stored answer.
4. `Engine.ScoreAndUpdate`, the one feature path.
5. `Scorer.Score` for the raw score, calibrated probability and `risk_score`, then Saabas reasons.
6. `RuleSet.Evaluate` on the rule set loaded at the start of the request.
7. Append to the decision log, and respond with `{assessment_id, decision, risk_score, matched_rule, reasons, ruleset_version}`.

### Hot swap

`PUT /v1/rules` parses, checks, lints and compiles a new rule set. On any error it returns every diagnostic and changes nothing. On success it swaps the new `*RuleSet` into an `atomic.Pointer`. A request loads the pointer once and uses that rule set for its whole evaluation, so it never sees half of one rule set and half of another, and a swap never takes a lock on the scoring path. Every decision records the rule-set version it used.

### Decision log and audit replay

Every decision is appended to a JSONL log with the full feature vector, the risk score, the rule-set version, matched shadow rules and the reasons. Because the rules are pure functions of the feature vector, any past decision can be re-evaluated from its log line against the rule set it used, and against a proposed one. That is also how shadow rules are compared with their backtests.

### Snapshots

Velocity state and the webhook deduper are snapshotted to disk and restored on start. The tests assert that the restored state equals the state before shutdown byte for byte, and that decisions after a restart match decisions from an uninterrupted run.

### Metrics

Latency histograms per endpoint (HdrHistogram), decisions by action and rule, rule-set version, state size, webhook verification failures by error code, and dedupe hits.

## Integration with Clearinghouse

Clearinghouse is the sibling project, a payments ledger in Ruby. It calls RiskGate on every payment confirm, and it reports outcomes back by webhook. The contract is written down in Clearinghouse's `docs/design/interfaces.md`.

**Assess.** `POST /v1/assess` with `{payment_id, created, amount, currency, risk_fields}`, headers `RiskGate-Deadline-Ms` and `Idempotency-Key: <payment_id>:<attempt>`. The response is `{assessment_id, decision, risk_score, matched_rule, reasons, ruleset_version}`. A non-200 answer or a timeout is `risk_unavailable` on Clearinghouse's side.

**Webhooks.** Clearinghouse delivers events at least once, with envelope `{id, object, type, created, api_version, data: {object}}`. RiskGate consumes `payment_intent.*` and `charge.dispute.*`, and a dispute becomes a fraud label for the backtester.

### Signatures

Each delivery carries `Clearinghouse-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>`, the same scheme as Stripe's webhook signatures. The full grammar is in `internal/webhook/signature.go`, and it is precise about the edges because the edges are where two implementations disagree. `t` appears exactly once and is 1 to 15 ASCII digits. The MAC covers `t` exactly as sent, so a leading zero is signed as sent. At least one `v1` must appear, no `v1` may be empty, and each is compared in constant time against the lowercase hex of the expected MAC. During a secret rotation the header carries one `v1` per active secret. Unknown schemes such as `v0` are ignored. The signature is checked before the timestamp, so a request without a valid signature learns nothing about the verifier's clock.

**Shared vectors.** RiskGate's Go verifier and Clearinghouse's Ruby verifier are separate code in separate languages, and they agree because both pass the same test vectors. `scripts/sync_signature_vectors.sh` copies Clearinghouse's vector file into `internal/webhook/testdata/clearinghouse_signatures.json`, and `TestClearinghouseSignatureVectors` runs every vector against the Go verifier. RiskGate's own 73 vectors are checked a third way in CI. `scripts/check_signature_vectors.sh` re-implements the header grammar in Python and computes every HMAC with the `openssl` CLI, so neither the vectors nor the Go code are checked only against themselves.

Running both verifiers on the shared vectors turned up seven places where the Go and Ruby verifiers disagreed before either shipped. Each was settled the same way. The behaviour was decided once, written into the grammar above, and pinned with a vector in both repositories, so the disagreement cannot come back unnoticed. In all seven, RiskGate's Go verifier moved to Clearinghouse's behaviour, which was the closer match to Stripe's:

| Vector | RiskGate before | Settled |
|---|---|---|
| `v1` in uppercase hex | accepted | `no_matching_signature`. The comparison is over lowercase hex bytes |
| `v1` not hex | `malformed_header` | `no_matching_signature`. A junk `v1` is a candidate that fails to match, not a broken header |
| `v1` one character short | `malformed_header` | `no_matching_signature` |
| `v1` one character long | `malformed_header` | `no_matching_signature` |
| `v1` containing `=` | `malformed_header` | `no_matching_signature` |
| `t` with leading zeros, signed as sent | rejected | valid. The MAC covers `t` as sent |
| `t` with too many digits | parsed, then `no_matching_signature` | `malformed_header` (1 to 15 digits) |

Running Clearinghouse's Go reference verifier over RiskGate's own 73 vectors found no further disagreements. The same review produced one change outside the vectors. A verifier holding an empty secret is refused at startup and fails closed, because anyone can compute an HMAC under an empty key, and forged dispute events would become fraud labels.

**An empty secret fails closed.** HMAC with an empty key is computable by anyone, so a verifier configured with no secrets, or with an empty secret beside a real one, must never accept anything. `Verifier.Validate` rejects that configuration, the service refuses to boot on it, and `VerifyAt` rejects every request if it is somehow reached anyway (`TestEmptySecretFailsClosed`). A misconfigured verifier that accepted everything would let anyone forge disputes and poison the labels, so this is the one place where failing closed is clearly right.

### Fail open, and what it costs

The opposite decision is made for the assess call. If RiskGate is down or misses its deadline, Clearinghouse proceeds with the payment and marks it `risk_unavailable`. That is fail open.

The argument for it is that a fraud check is a filter on revenue, not a safety interlock. Failing closed turns a RiskGate outage into a total payment outage for every merchant, and most payments in any window are legitimate. The argument against it is real too. An attacker who can degrade RiskGate gets an unchecked window, and every payment in it is exposed. Experiment 9 measures that window instead of arguing about it. It kills RiskGate during a replay, counts the payments that went through unchecked, checks that Clearinghouse's own p99 stays bounded by the deadline plus overhead, restarts RiskGate from its snapshot, and shows decisions resume. The deadline and the circuit breaker live on Clearinghouse's side.

## Experiments

Every experiment states its machine, which is an Apple M3 Pro, with core counts, Go version and `GOMAXPROCS`. Every number on the IEEE-CIS test month comes from one run at the end. Anything simulated says so where it appears.

### 1. What the streaming features are worth

**Question.** How much do RiskGate's velocity features add over the raw transaction columns?
**Method.** Four baselines on the time split, test month touched once. Fraud-dollar recall is measured at a fixed budget of legitimate dollars blocked, chosen on validation.
**Result.**

| Model | ROC-AUC | PR-AUC | Fraud recall at 1% FPR | Fraud-dollar recall at legit-dollar budget | Brier | ECE |
|---|---|---|---|---|---|---|
| Rules alone | TBD (experiment 1) | n/a | TBD (experiment 1) | TBD (experiment 1) | n/a | n/a |
| Logistic regression | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) |
| LightGBM, raw columns | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) |
| LightGBM, raw plus velocity | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) | TBD (experiment 1) |

Context, said once. The competition's winning private-leaderboard AUC was 0.945884, on Kaggle's own test set with months of feature engineering. RiskGate uses a different split and a different goal. It is not competing with that number and does not compare itself to it. The claim is the ablation, not a rank.

### 2. Train/serve parity

**Question.** Is the model in production the model that was evaluated?
**Method.** Replay the test month through the HTTP service, capture every feature vector, and compare with the offline export row by row. Separately, compare Go raw scores with LightGBM's on every test-month row (`cmd/parity`).
**Result.**

| Check | Rows compared | Rows that differ | Largest absolute difference |
|---|---|---|---|
| Service features against export | TBD (experiment 2) | TBD (experiment 2) | TBD (experiment 2) |
| Go raw score against LightGBM | TBD (experiment 2) | TBD (experiment 2) | TBD (experiment 2) |

### 3. Two evaluators, one answer

**Question.** Do the closure and vectorized evaluators agree on every rule and every row?
**Method.** `backtest.DiffTest` over generated, grounded rules on the full table, plus the rule-set level check, plus parser fuzzing.
**Result.**

| Rules generated | Rows per rule | Rule-row pairs checked | Disagreements | Bugs found |
|---|---|---|---|---|
| TBD (experiment 3) | TBD (experiment 3) | TBD (experiment 3) | TBD (experiment 3) | TBD (experiment 3) |

### 4. Velocity state shootout

**Question.** What does bounding memory cost in accuracy, speed, and fraud caught?
**Method.** Exact, bucketed and sketch states. Memory and speed from benchmarks, error from `features.CompareStates`, and the effect on experiment 1's metrics by retraining on each state's export (`cmd/export -state`).
**Result.**

| State | Bytes per key | ns per update | ns per read | Features exact (share of rows) | Worst feature error | Fraud recall at 1% FPR |
|---|---|---|---|---|---|---|
| Exact | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | 1 (reference) | 0 (reference) | TBD (experiment 4) |
| Bucketed ring | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) |
| Sketch | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) | TBD (experiment 4) |

### 5. Latency under load

**Question.** What is the highest arrival rate at which p99 stays inside the deadline?
**Method.** `cmd/loadgen`, open loop at fixed rates, latency measured from each request's intended send time (see `docs/LOADGEN.md`), HdrHistogram percentiles. Sweep the shard count and compare with a single mutex and `sync.Map`.
**Result.** Machine `TBD`. Deadline `TBD (starts at 50 ms)`.

| State concurrency | Rate (req/s) | p50 | p99 | p99.9 | p99 inside deadline |
|---|---|---|---|---|---|
| Single mutex | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) |
| `sync.Map` | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) |
| Sharded, N = TBD | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) | TBD (experiment 5) |

Highest rate with p99 inside the deadline `TBD (experiment 5)`.

### 6. Backtest speed

**Question.** Is a backtest fast enough that an analyst iterates?
**Method.** `backtest.RunBench`. One rule and the 50-rule `RealisticRuleSet` over the full table, vectorized against row-at-a-time on the same cached features, one thread and `GOMAXPROCS`. Row-at-a-time gets a pre-built row store for free, which flatters it.
**Result.**

| Workload | Evaluator | Threads | Best time | Rows per second |
|---|---|---|---|---|
| One rule | Vectorized | 1 / GOMAXPROCS | TBD (experiment 6) | TBD (experiment 6) |
| One rule | Row-at-a-time | 1 / GOMAXPROCS | TBD (experiment 6) | TBD (experiment 6) |
| 50-rule set | Vectorized | 1 / GOMAXPROCS | TBD (experiment 6) | TBD (experiment 6) |
| 50-rule set | Row-at-a-time | 1 / GOMAXPROCS | TBD (experiment 6) | TBD (experiment 6) |

### 7. Label delay (simulated)

**Question.** How far does a naive recent-window backtest understate a rule's fraud catch, and does the maturity window fix it?
**Method.** Simulate a dispute arrival time for each fraud label from `DefaultDelay` (lognormal, median 30 days, sigma 0.5, capped at 120 days, an assumption and not a measurement). Compare a naive last-30-days backtest and a matured one against the truth (`Backtester.CompareLabelDelay`).
**Result.** Every number here is SIMULATED.

| Backtest | Fraud caught as reported | Fraud caught in truth | Understatement |
|---|---|---|---|
| Naive, last 30 days | TBD (experiment 7) | TBD (experiment 7) | TBD (experiment 7) |
| Matured, 60-day maturity | TBD (experiment 7) | TBD (experiment 7) | TBD (experiment 7) |

### 8. End to end through Clearinghouse

**Question.** What does the whole system do to losses on real transaction amounts and real labels?
**Method.** Replay the test month through Clearinghouse's API, with Clearinghouse calling RiskGate on every confirm. Fraud-labelled payments use Clearinghouse's disputing test card, so disputes arrive in simulated time and flow back as labels by webhook. Compare three policies. Everything around the data is a simulation driven by it, and the per-dispute fee is an assumption.
**Result.**

| Policy | Disputes | Dispute losses (amount plus assumed fee) | Legitimate revenue blocked | Net |
|---|---|---|---|---|
| No risk checks | TBD (experiment 8) | TBD (experiment 8) | TBD (experiment 8) | TBD (experiment 8) |
| Model threshold alone | TBD (experiment 8) | TBD (experiment 8) | TBD (experiment 8) | TBD (experiment 8) |
| Model plus rules | TBD (experiment 8) | TBD (experiment 8) | TBD (experiment 8) | TBD (experiment 8) |

Assumed per-dispute fee `TBD`.

### 9. The risk service fails

**Question.** What does failing open actually cost?
**Method.** Kill RiskGate during experiment 8's replay, then restart it from its snapshot.
**Result.**

| Payments during outage | Went through unchecked | Of those, fraud | Clearinghouse p99 during outage | Time to first decision after restart |
|---|---|---|---|---|
| TBD (experiment 9) | TBD (experiment 9) | TBD (experiment 9) | TBD (experiment 9) | TBD (experiment 9) |

## Bug log

Real bugs found by tests, fuzzing and parity checks, each with a regression test.

- [`internal/rules/BUGLOG.md`](internal/rules/BUGLOG.md). Two parser bugs found by `FuzzParse`. The checker was quadratic in nesting depth (a 20,000-deep `is_missing` chain took 7.0 s to load and now takes 58 ms), and junk input produced one diagnostic per byte.
- `internal/backtest/BUGLOG.md`, for the vectorized evaluator and the differential test. `TBD: not written yet.`
- **The model evaluator.** LightGBM's zero threshold is `1e-35f`, not `1e-35` (see [the Go evaluator](#the-go-evaluator)). Found by reading `meta.h` while writing the fixture generator, and pinned by fixtures that probe the values between the two.
- **Calibration parity.** `np.interp`'s last bit depends on whether NumPy's build fuses a multiply-add. Fixed by writing the interpolation step by step in both languages.
- **Export parity.** pandas' default float parser can be one ulp off. The Python side reads the export with `float_precision="round_trip"`.

## What I would do next

**Multi-tenancy.** RiskGate holds one merchant's rules. Tenancy would key three things by account. Rule sets become a map from account id to `atomic.Pointer[RuleSet]`, so one merchant's swap never touches another's. Named lists are per account. Velocity state gets the account id as part of every key, which isolates merchants by default. Network-wide signals, such as a card seen failing at many merchants, would be a separate, deliberately shared entity rather than a leak between tenants. The model would stay shared, and `risk_score` would mean the same thing for everyone, which is what lets one merchant's threshold be advice to another. Backtests would read only the tenant's rows. The hard part is not the code but fairness of the shared state. A merchant under a card-testing attack should not evict everyone else's keys, so memory limits would need to be per tenant.

**3D Secure, the fourth action.** Radar evaluates request-3DS rules before allow rules. A 3DS rule asks the cardholder's bank to authenticate the payment, which moves fraud liability and adds friction. RiskGate leaves it out because real 3DS needs a card network and an issuer, and a simulated challenge would be a guess about how often customers abandon a payment, not a result. The rule language already reserves the place. It would be one more action evaluated first, with one more outcome for the backtester to report.

**Other things, in order.**
- Label arrival times from Clearinghouse's webhooks instead of a simulation, which experiment 8 starts to provide.
- Drift monitoring on `risk_score`'s calibration, since a threshold means what it means only while the calibration holds.
- A rule-coverage view across the whole rule set, extending the unreachable-rule check from one rule to all of them.

## Reading list and credits

Each entry says what RiskGate took from it. Quotes are short and exact.

- **Stripe, "Fraud prevention rules"** (Radar rules documentation). The rule shape `{action} if {condition}`, the `:attribute:` and `@list` syntax RiskGate borrows, and backtesting over past charges. https://docs.stripe.com/radar/rules
- **Stripe, Radar rules reference.** The evaluation order ("Rules of the same action type aren't ordered"), the operators and functions, and the "Missing attributes" section RiskGate follows for comparisons and departs from for `not`. https://docs.stripe.com/radar/rules/reference
- **Stripe, "Test and backtest your rules"** (Radar testing). The backtest summary's categories, and overrides for allow rules ("when you test allow rules, you can also see Overrides"). https://docs.stripe.com/radar/testing
- **Stripe, "Radar for Fraud Teams: Rules 101".** The same order from the fraud team's side, where allow rules "override all of your other rules except 3DS rules". https://stripe.com/guides/radar-rules-101
- **IEEE-CIS Fraud Detection** (Kaggle, 2019, sponsored by the IEEE Computational Intelligence Society, data from Vesta Corporation). The dataset, and its rules. https://www.kaggle.com/competitions/ieee-fraud-detection and https://www.kaggle.com/competitions/ieee-fraud-detection/rules
- **NVIDIA Technical Blog, "Leveraging Machine Learning to Detect Fraud"** (by members of the winning team). The dataset's class balance, "only 3.5% of the transactions are labeled fraudulent". https://developer.nvidia.com/blog/leveraging-machine-learning-to-detect-fraud-tips-to-developing-a-winning-kaggle-solution/
- **Chris Deotte and Konstantin Yakovlev (team FraudSquad), first-place solution, parts 1 and 2.** The `uid` reconstruction from `card1`, `addr1` and `D1`, which RiskGate uses as one entity key. Part 1 names "the three columns card1, addr1, and D1". The winning private-leaderboard AUC, 0.945884, is read from the competition leaderboard and quoted only as context. https://www.kaggle.com/competitions/ieee-fraud-detection/discussion/111284 and https://www.kaggle.com/competitions/ieee-fraud-detection/discussion/111308
- **Martin Kleppmann, *Designing Data-Intensive Applications*** (O'Reilly, first edition, 2017), chapter 11, "Stream Processing". Event time against processing time, and window types. The second edition (2026) renumbers the chapters, so the citation is to the first. https://www.oreilly.com/library/view/designing-data-intensive-applications/9781491903063/ch11.html
- **Ando Saabas, "Interpreting random forests"** (2014), and the `treeinterpreter` package. The contribution method behind RiskGate's reasons, "a sum of feature contributions". http://blog.datadive.net/interpreting-random-forests/
- **Scott Lundberg, Gabriel Erion and Su-In Lee, "Consistent Individualized Feature Attribution for Tree Ensembles"** (2018). TreeSHAP, and the argument that "the gain, split count, and Saabas methods are all inconsistent". https://arxiv.org/abs/1802.03888
- **Gil Tene, "How NOT to Measure Latency".** Coordinated omission, which `cmd/loadgen` is built to avoid. https://www.infoq.com/presentations/latency-response-time/
- **HdrHistogram**, and its Go port used for every latency percentile here. https://hdrhistogram.org/ and https://github.com/HdrHistogram/hdrhistogram-go
- **Graham Cormode and S. Muthukrishnan, "An improved data stream summary: the count-min sketch and its applications"** (Journal of Algorithms 55(1), 2005). The count and sum sketch. https://doi.org/10.1016/j.jalgor.2003.12.001
- **Philippe Flajolet, Éric Fusy, Olivier Gandouet and Frédéric Meunier, "HyperLogLog: the analysis of a near-optimal cardinality estimation algorithm"** (AofA 2007, DMTCS Proceedings). The distinct-card estimate. https://doi.org/10.46298/dmtcs.3545
- **Vaughan Pratt, "Top Down Operator Precedence"** (POPL 1973). The parser. https://doi.org/10.1145/512927.512931
- **LightGBM**, whose text model format, `tree.h` and `meta.h` define what the Go evaluator must match. https://github.com/microsoft/LightGBM
