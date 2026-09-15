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
- **`card1` is coarse.** Many customers share a `card1` value, which is why `uid` exists at all. On this data, distinct-card counts are higher for legitimate payments than for fraud, because a shared `card1` collects many cards' worth of traffic.
- **`device_info` is not a device.** It is a model or OS string, such as "Windows" or a phone model. It is missing on 80% of payments, and where it exists many unrelated customers share it. So device velocity and `distinct_cards_per_device_24h` measure traffic from a device type, not from one machine, and the textbook card-testing rule is weak here. Rules still get the field, because it is what the data has.
- **`uid` is missing where fraud concentrates.** Nearly every product-C payment lacks `billing_region` (addr1), so its `uid` is missing. Product C carries 38% of the fraud.
- **Labels are mature, and have no arrival time.** `isFraud` comes from chargebacks and was assigned after the fact. A real system learns about fraud weeks later. The data does not say when each label arrived, so experiment 7 simulates arrival times from a stated assumption and labels every number it produces as simulated.
- **Time is an offset.** `TransactionDT` counts seconds from an undisclosed reference. Where a calendar date is printed, RiskGate assumes the reference is 2017-12-01 00:00 UTC, the date commonly assumed in the competition's discussion (`backtest.DefaultEpoch`, `data.ReplayEpochUnix`). Nothing depends on that date being right, only on it being fixed.
- **Evaluation uses a time split.** The Kaggle test file has no labels, so all evaluation splits the labelled file by time. `TransactionDT` spans about 182 days. `internal/data/split.go` cuts it into 30-day months aligned to midnight. Months 0 to 3 train, month 4 is validation, month 5 is test, and the partial seventh month (about two days) is folded into test rather than dropped. Random splits would put a customer's later payments in training and their earlier ones in test, which is leakage by another name.

## Data license

The IEEE-CIS data is covered by the competition's rules on Kaggle. Section 7 of the general competition rules, "Competition Data", allows use "for non-commercial purposes only", including academic research and education, and asks participants not to redistribute the data to anyone who has not accepted the rules. This project is non-commercial and educational, so using the data is fine. Redistributing it is not, and that shapes what this repository contains.

- **Raw data is never committed.** `scripts/fetch_data.sh` downloads it with your own Kaggle credentials after you accept the rules yourself. `data/` is in `.gitignore`.
- **Row-level derived data is never committed or published either.** That covers the binary cache (`data/cache/*.rgc`), the feature export (`export.csv`) and backtest table (`features.table`, `*.rgt`), the Clearinghouse replay file (`test_replay.jsonl`), decision logs, and the sample payments a backtest report shows. A feature table is the dataset with extra columns, and a replay file is the dataset in JSON, so the same rule applies to them.
- **Only aggregate results are published.** AUCs, counts, dollar totals, latency percentiles, and plots. A number in this document or the README never identifies a transaction.
- **Tests run on synthetic data.** `cmd/synth` writes an IEEE-CIS-shaped dataset from a seeded generator, and `cmd/synth`, `cmd/export`, `riskgate table` and the service's page label everything built from it `SYNTHETIC` (`data.SyntheticMarker`). The model parity fixtures under `internal/model/testdata` are small models trained on generated inputs.
- **The page and the API show sample payments only locally.** A backtest report includes up to ten matching payments so an analyst can see what a rule catches. That is fine on your machine against data you downloaded. It is not fine in a published screenshot, so the demo recording uses synthetic data.

## Architecture

```
            Clearinghouse (Ruby) confirm path                Rule author (browser)
                     |  POST /v1/assess                              |
                     |  (deadline set by Clearinghouse)              |
                     v                                               v
  +-------------------------------------------+      +---------------------------+
  |  RiskGate service (Go)                    |      |  Backtest page and API    |
  |                                           |      |  (in the service, and     |
  |  features.Engine ---- velocity state      |      |   cmd/backtest)           |
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
| `internal/features` | The feature engine, three velocity states, three concurrency wrappers, snapshots, replay | built |
| `cmd/export` | Offline feature export for training, plus the test-month replay file | built |
| `internal/model` | LightGBM text-model evaluator, Saabas contributions, calibration, reasons | built |
| `python/` | Offline training, baselines, evaluation, parity scores, fixtures | built |
| `cmd/parity` | Go raw scores against LightGBM's on every exported row | built |
| `internal/rules`, `cmd/rulecheck` | Lexer, Pratt parser, checker, linter, printer, closure compiler, generator | built |
| `internal/backtest` | Columnar table, vectorized evaluator, differential test, reports, sweep, label delay | built |
| `cmd/backtest` | Command line for backtests, the sweep, and experiments 3, 6 and 7, over `cmd/export`'s `features.table` or `riskgate table`'s scored table | built |
| `internal/webhook` | Clearinghouse signature verifier, dedupe, event envelope, HTTP handler | built |
| `internal/loadgen`, `cmd/loadgen` | Open-loop load generator with coordinated-omission correction | built |
| `internal/service`, `cmd/riskgate` | `/v1/assess`, idempotency, rule swap, shadow rules, decision log and `riskgate audit`, snapshots, metrics, the page, and `riskgate table` | built |

## One feature implementation, and point-in-time correctness

Train/serve skew is the most common way a fraud model fails quietly. The model is trained on features computed one way, usually a batch job in Python or SQL, and served on features computed another way, usually a streaming service written later by someone else. The two disagree at a window edge, on a null, or on whether "the last hour" includes the current payment. Nothing crashes. The model just gets worse, and nobody can say by how much.

RiskGate rules this out by construction. There is one function that turns a payment and the current state into a feature vector, `Engine.ScoreAndUpdate` (`internal/features/engine.go:171`). The offline export calls it through `features.Replay`. The backtester's table is built from the same replay (`features.ReplayColumns` produces exactly the column layout it scans). The online service calls it once per request. Python never computes a feature. It reads the Go export (`python/riskgate.py`), and it even parses floats with `float_precision="round_trip"` because pandas' default parser can be one ulp off, which would break parity before the model is involved.

Construction is a claim. Experiment 2 is the proof. It replays all 590,540 payments through the HTTP service, captures every feature vector, and compares it with the offline export row by row.

### Point in time

A feature for a payment at time *t* may use only payments strictly before it. Three things enforce that.

1. **Score before update.** `ScoreAndUpdate` reads the state, writes the row, and only then adds the payment. A payment never sees itself. Each key is read and updated under one lock (`State.ReadAdd`), so two concurrent payments on the same key each see the other entirely or not at all.
2. **A total order.** Replay feeds payments in `(TransactionDT, TransactionID)` order (`data.Less`, `internal/data/txn.go:30`). IEEE-CIS has many payments in the same second, and "strictly before" is ambiguous for them unless the order is total. The tie-break by `TransactionID` makes it total and deterministic. A payment at the same second with a larger id counts as later. `Replay` checks the order as it goes and fails on a violation, because an out-of-order input would leak the future silently.
3. **Windows exclude the present.** Every count and sum covers strictly earlier payments of the same entity inside the trailing window. The schema's field docs say so, and `TestWindowEdges` pins the boundary to the second.

### The leakage test

`TestLeakage` (`internal/features/leakage_test.go:35`) checks the property directly instead of trusting the three mechanisms above. For each sampled payment *i* it rewrites history after *i*. It mutates or deletes every later event and inserts new ones, including payments at the same second with a larger `TransactionID` and payments on the same card and device. Then it shuffles the whole input, runs the same pipeline the export runs (`data.Sort`, then `Replay`), and requires the features of *i*, and of everything before it, to be bit-identical to the untouched run. It runs against all three state implementations. CI runs a small sample under the race detector, and one environment variable runs it over thousands of payments on a full dataset.

### Event time, not processing time

Windows are defined in event time, the payment's own `created` timestamp, never the wall clock of the machine computing the feature. Replay has no wall clock at all. Online, Clearinghouse sends `created`, and the service converts it back to `TransactionDT` (`data.DTFromUnix`), so a replayed month computes the same windows at any replay speed. Kleppmann's chapter on stream processing is the reference here.

The cost of event time is out-of-order arrival. Online, two requests for the same card can arrive in the opposite order from their timestamps. RiskGate's rule is that a late event is recorded at the key's latest time rather than its own (`TestLateEventIsClamped`). It is still counted, and a key's history stays in time order, so windows never have to be repaired. The trade-off is that a late event can stay in a window for up to a few seconds longer than it should. I chose that over buffering, because a buffer would add latency to every payment to fix a rare edge. A late payment is also scored as of that latest time, so its `seconds_since_last` is 0, never the negative value training never sees (`TestLateEventIsScoredAtKeyLatest`). Replay never produces a late event, so offline rows are unchanged.

One more honest limit. Per-key atomicity is all the engine promises. Two concurrent payments that share a card but not a device can see each other through the card and not through the device. Replay is sequential and never hits this. Online it means features depend on arrival order within a few milliseconds, which is also true of any real system that does not serialize all payments.

## Velocity state

For each entity (card, `uid`, device, email domain) and each window (1 hour, 24 hours, 7 days), RiskGate keeps the payment count and dollar sum. From those it derives the 7-day mean, the ratio of this amount to that mean, seconds since the entity was first seen and since its previous payment, and distinct cards per device and per email domain in 24 hours, which is the card-testing signal. The field list is generated in `schema.VelocityFieldNames`.

The state has to be bounded in memory, fast under concurrency, and correct at window edges. There are three implementations behind one `State` interface, and experiment 4 measures them against each other.

| State | How | Memory | Error |
|---|---|---|---|
| **Exact** (`NewExact`) | A per-key deque of every event in the last 7 days, with running totals per window. Reads walk only the events that expired since the last add. | Grows with traffic. A card-testing burst on one device grows that device's deque for a week. | None. It is the reference every other state is measured against, and `TestExactMatchesOracle` checks it against a brute-force oracle. |
| **Bucketed ring** (`NewBucketed`) | Per key and window, a ring of fixed time buckets (1-minute for the hour, 15-minute for the day, hourly for the week), storing only non-empty buckets. Distinct cards stop growing at a cap of 1,024. | Bounded per key by the bucket count, however many payments the key makes. | Only at the trailing edge. Counts and sums never undercount and overcount by at most one bucket's contents. A saturated distinct count reports the cap. |
| **Sketch** (`NewSketch`) | Count-min sketches per time bucket for counts and sums, HyperLogLog registers for distinct cards, and a fixed-size table of full 64-bit key hashes for first and last seen. Shared by all keys. | Fixed at construction, independent of the number of keys. | Counts never undercount (`TestSketchNeverUndercounts`). Collisions push estimates up by an amount that grows with total traffic in the window, not the key's own. First and last seen can only err by forgetting a key early, never by inventing one. |

The sketch's error is reported, not hidden. `features.CompareStates` replays history through the exact state and an approximate one in lockstep and reports each feature's error. More important than per-feature error is what the approximation does to the thing people care about, fraud caught at a fixed false-positive rate. Experiment 4 computes every row's features with each state, scores the validation month with the one trained model, and reports that. If the sketch costs nothing measurable, that is a finding. If it does, that is also a finding.

Idle keys are evicted after 30 days (`DefaultIdleTTL`). Eviction only frees memory. An idle key already reads as unseen, so results do not depend on when a sweep runs.

For concurrency, `NewSharded` hashes keys across N states, each behind its own read-write mutex and padded to Apple silicon's 128-byte cache line. `NewLocked` (one mutex) and `NewSyncMap` (per-key entries in a `sync.Map`) are the baselines. The key hash is FNV-1a with a finalizer, deliberately not `hash/maphash`, because shard assignments and sketch cells are written into snapshots and must mean the same thing after a restart. Experiment 5 sweeps N.

Snapshots are a small binary format, deterministic in order, so equal states produce byte-identical snapshots. That makes "the restored state equals the state before shutdown" a byte comparison (`TestSnapshotRestore`).

The service runs the exact state by default (`riskgate serve -state exact`). Experiment 4 found it the smallest of the three at this data's scale, with no error, and the bucketed ring costs nothing measurable on the model's metrics if a bound per key is needed. The shard count is still open, because experiment 5's only run was on a loaded machine.

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

**Parity.** The target is bit-identical raw scores, not "close". The unit tests compare against LightGBM's `predict(raw_score=True)` on fixtures built to break an evaluator that is merely close (`python/gen_fixtures.py`). Rows are fed at every split threshold, one ulp above and below it, at both signed zeros, at values inside LightGBM's zero threshold, and at NaN and infinities. Categorical fixtures probe codes past the bitset, negative codes and fractional codes. On real data, `cmd/parity` compares every row of the test month against scores written by `python/parity.py` and exits 1 on any difference. On the test month all 92,427 rows were bit-identical ([experiment 2](#2-trainserve-parity)).

Two findings from building it are worth recording, because both are the kind of thing that makes an evaluator agree on 99.99% of rows.

- **LightGBM's zero threshold is a float literal.** `kZeroThreshold` in LightGBM's `meta.h` is `1e-35f`. Its double value is `float32(1e-35)`, about `1.0000000180025095e-35`, not `1e-35`. An evaluator that writes `1e-35` sends a value between the two down the wrong branch of a zero-as-missing split. `internal/model/lightgbm.go:39` spells it `float64(float32(1e-35))`, and the fixtures probe exactly those values.
- **`np.interp` is not reproducible to the last bit.** NumPy's C loop computes `slope*(x-xp[j]) + fp[j]`, and whether the compiler fuses that into one multiply-add depends on how NumPy was built. The arm64 macOS wheel fuses, a baseline x86-64 build cannot. So the calibration step is written operation for operation in both languages. In Go the `float64(...)` conversion forbids fusion (the Go spec allows fusing only when no explicit conversion intervenes). In Python each step is its own NumPy call (`riskgate.interpolate`). `TestCalibratorMatchesPython` checks the two bit for bit.

### Reasons, and why they are Saabas and not SHAP

Each decision carries up to three reasons, such as "9 payments on this card in the last hour, usually fewer than 2". They come from per-feature contributions computed with the **Saabas** method, from Ando Saabas's `treeinterpreter`. Follow the path the payment takes through each tree. Every time the path moves from a node to a child, credit the change in the node's expected value to the feature that node split on. A tree's contributions telescope to its leaf value minus its root value, so the bias plus all contributions equals the raw score (`TestSaabasSumsToRawScore`).

This is not SHAP, and the difference matters. TreeSHAP (Lundberg, Erion and Lee) averages a feature's marginal contribution over every order in which features could be revealed. That makes it consistent, meaning a model that relies more on a feature never gives it less credit, and fair to interacting features. Saabas credits only the one order the tree happens to test features in, so it is path-dependent and biased toward features split near the root. Lundberg et al. use Saabas as their example of an inconsistent method.

RiskGate uses Saabas anyway because it comes from the same walk of each tree that produces the score (`Model.PredictContributions`, whose raw score keeps `PredictRaw`'s exact bits, checked by `TestPredictContributionsExact`), so explaining a payment costs little more than scoring it, needs no allocation, and answers the only question a reason string asks, which features pushed this payment's score up. It is not used for anything that needs consistency, such as global feature importance. LightGBM's `pred_contrib` output is TreeSHAP and will not match these numbers, and nothing in RiskGate calls them SHAP values.

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

On the real data: 10,000 generated rules over all 590,540 rows, 5.9 billion rule-row checks, 0 disagreements ([experiment 3](#3-two-evaluators-one-answer)). Bugs the fuzzer found are in the [bug log](#bug-log).

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

This section describes `internal/service` and `cmd/riskgate serve`.

### `POST /v1/assess`

1. Decode the payment. Unknown `risk_fields` keys and wrongly typed values are errors, not ignored (`data.ParseRiskFields`). A typo such as `card_1` would otherwise become a missing value online while the export had the real one, which is train/serve skew arriving through the API.
2. Honor the caller's deadline from `RiskGate-Deadline-Ms`. If the budget is already spent, answer immediately rather than do work nobody will wait for.
3. Deduplicate by `Idempotency-Key`. Clearinghouse retries on timeout, and a retry must not count the payment twice in the velocity state. The same key gets the same stored answer.
4. `Engine.ScoreAndUpdate`, the one feature path.
5. `Scorer.Score` for the raw score, calibrated probability and `risk_score`, then Saabas reasons.
6. `RuleSet.Evaluate` on the rule set loaded at the start of the request.
7. Append to the decision log, and respond with `{assessment_id, decision, risk_score, matched_rule, reasons, ruleset_version}`.

A `created` far in the future would become its keys' latest time and hold every later payment on them, including shared keys like an email domain, inside every window. So the service answers 400 `created_out_of_range` for a value after the year 3000, which is usually milliseconds, and 400 `created_in_future` for a value more than `-max-future-skew` (default 24h, 0 turns it off) ahead of both the latest `created` it has accepted and the wall clock. The clock gives the first payment a reference, and a replay of past data in order is always accepted.

### Hot swap

`PUT /v1/rules` parses, checks, lints and compiles a new rule set. On any error it returns every diagnostic and changes nothing. On success it swaps the new `*RuleSet` into an `atomic.Pointer`. A request loads the pointer once and uses that rule set for its whole evaluation, so it never sees half of one rule set and half of another, and a swap never takes a lock on the scoring path. Every decision records the rule-set version it used.

### Decision log and audit replay

Every decision is appended to a JSONL log with the full feature vector, the risk score, the rule-set version, matched shadow rules, the reasons, and the model's `model_sha256`. That is the SHA-256 of the model directory's four files, also shown in `/v1/info` and `/metrics`, and `riskgate audit` refuses to replay a log against a model with a different hash. Because the rules are pure functions of the feature vector, any past decision can be re-evaluated from its log line against the rule set it used, and against a proposed one. That is also how shadow rules are compared with their backtests.

### Snapshots

Velocity state and the webhook deduper are snapshotted to disk and restored on start. A snapshot taken under traffic copies the idempotency store after the velocity state, inside a brief barrier that each keyed assess holds from its velocity update until its answer is stored. So a retry after a crash is never counted twice. The tests assert that the restored state equals the state before shutdown byte for byte, and that decisions after a restart match decisions from an uninterrupted run.

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

Test month (month 5 plus the two-day tail): 92,427 payments, 3.48% fraud. Evaluated once, on 2026-09-23, by `python/evaluate.py`, which wrote `results/ieee/test_touched.lock` before reading a single test label. Full output in [`results/ieee/test_metrics.md`](results/ieee/test_metrics.md). The legitimate-dollar budget is 1% of all legitimate dollars in the test month.

| Model | ROC-AUC | PR-AUC | Fraud recall at 1% FPR | Fraud-dollar recall at 1% legit-dollar budget |
|---|---|---|---|---|
| Rules alone (`rules/baseline.rules`, 8 rules, no `risk_score`) | n/a | n/a | 28.3% recall at 5.72% FPR, its one operating point | 25.8% at 8.56% of legit dollars |
| Logistic regression | 0.7947 | 0.1134 | 0.5% | 0.9% |
| LightGBM, raw columns | 0.7745 | 0.2016 | 14.8% | 15.6% |
| **LightGBM, raw plus velocity** | **0.8105** | **0.2275** | **16.8%** | **20.5%** |

**What it says.** At the same 1% false-positive rate, the velocity features catch 16.8% of fraud instead of 14.8%, and at the same legitimate-dollar budget they catch 20.5% of fraud dollars instead of 15.6%, about a third more. ROC-AUC rises from 0.7745 to 0.8105. That is a real gain, and a modest one. The honest reading has three parts. First, everything is lower on test than on validation (validation ROC-AUC was 0.8450 for the full model), which is what a time split is supposed to reveal. Second, the absolute numbers are low because RiskGate's model sees only the eleven interpretable raw fields a rule author can name, plus its own velocity features. It deliberately leaves out the hundreds of anonymous Vesta columns (`C1`-`C14`, `D`, `M`, `V`) that carry most of the signal in competition solutions, because a rule cannot name them and a reason cannot explain them. Third, the velocity features are weaker than they would be on data with real identities. `device_info` is a model string such as "Windows", not a device, `card1` is shared by many customers, and `uid` is missing for nearly every product-C payment, where 38% of the fraud is. Experiment 4 found these by looking.

**The rules baseline** was tuned by hand on the validation month (the log is [`results/rules_baseline/TUNING.md`](results/rules_baseline/TUNING.md)), so its validation numbers are optimistic. On test it catches more fraud than the model's 1%-FPR operating point, at over five times the false-positive rate, and it has no threshold to move. That is the argument for the design in this document: rules are how an analyst expresses a policy, and `risk_score` is how the model's ranking becomes one more attribute a rule can use.

**Calibration** of the served model on test: isotonic calibration (fitted on validation) cut the expected calibration error from 0.0071 to 0.0038 over 10 equal-width bins, and from 0.0066 to 0.0057 over 20 equal-count bins. The Brier score was unchanged (0.0302 to 0.0303). The reliability plot is `results/ieee/reliability.png`.

Context, said once. The competition's winning private-leaderboard AUC was 0.945884, on Kaggle's own test set with months of feature engineering. RiskGate uses a different split and a different goal. It is not competing with that number and does not compare itself to it. The claim is the ablation, not a rank.

### 2. Train/serve parity

**Question.** Is the model in production the model that was evaluated?
**Method.** A fresh `riskgate serve` (models/ieee, `rules/default.rules`, exact state with 64 shards, no snapshot) received all 590,540 IEEE-CIS transactions over HTTP, one request at a time in (TransactionDT, TransactionID) order, so its velocity state saw the same history the offline export replayed. Labels were not sent. `cmd/serveparity compare` then checked every decision-log line three ways, comparing bits (`math.Float64bits`, with missing equal to missing): all 53 catalog features against a fresh offline `features.Replay`, the 53 encoded model inputs against `export.csv`, and the logged raw score, probability and `risk_score` against the offline Scorer. Separately, `python/parity.py` (which reads no labels) and `cmd/parity` compared Go raw scores with LightGBM's on every test-month row. `riskgate audit` replayed the decision log. As a negative control, a copy of the log with one amount moved by one ulp and one line deleted must fail the check, and it does (`scripts/experiments/exp2_negative_control.sh`). Run with `scripts/experiments/exp2.sh`. Results are in `results/exp2/`.
**Result.** Apple M3 Pro, go1.26.5, 2026-09-23, on a shared machine with a load average of 12 to 21. Load affects only wall time, not these results.

| Check | Rows compared | Rows that differ | Largest absolute difference |
|---|---|---|---|
| Service features against export, all months | 590,540 | 0 | 0 |
| Service features against export, test month | 92,427 | 0 | 0 |
| Service raw score against offline Scorer, all months | 590,540 | 0 | 0 |
| Go raw score against LightGBM, test month (954 trees) | 92,427 | 0 (all 92,427 bit-identical) | 0 |
| `riskgate audit` of the decision log | 590,540 | 0 | n/a |

No mismatches, so no bugs to log (`results/exp2/BUGS.md`). The decision log dropped 0 records during the replay.

### 3. Two evaluators, one answer

**Question.** Do the closure and vectorized evaluators agree on every rule and every row?
**Method.** `backtest difftest -rules 10000` on the real IEEE-CIS table, every row. Half the rules are generated as they come, half with constants drawn from the table. It ran twice: on the exported table (`risk_score` missing on every row) and on the same replay scored by the trained model. The rule-set level check (`backtest.CheckRuleSet`, action and deciding rule on every row) covers the 50-rule `RealisticRuleSet`, `rules/baseline.rules` and `rules/default.rules`, on both tables. Parser fuzzing is separate (see the bug log). Details are in `results/difftest_real/`.
**Result.**

| Table | Rules generated | Non-trivial rules | Rows per rule | Rule-row pairs checked | Disagreements | Bugs found |
|---|---|---|---|---|---|---|
| Exported (`risk_score` missing) | 10,000 | 5,576 | 590,540 | 5,905,400,000 | 0 | 0 |
| Scored by the model | 10,000 | 5,676 | 590,540 | 5,905,400,000 | 0 | 0 |

A non-trivial rule matches some rows but not all. The rest match none or all, which exercises missing values and little else. At the rule-set level, all three sets had 0 mismatches on all 590,540 rows of both tables.

### 4. Velocity state shootout

**Question.** What does bounding memory cost in accuracy, speed, and fraud caught?
**Method.** `cmd/experiments/state` on the real IEEE-CIS data, all three states at default settings. Memory is each state's own estimate after a full replay, checked against the measured heap. Speed is ns per state operation over the data's own 1.7M keyed updates, on one goroutine, median of 7. Error comes from `features.CompareStates` against exact, on every row. For the model metrics, the model trained on exact-state features (`models/ieee`, not retrained) scores the validation month, each state computing the features through the same replay, stopped before the test month. That is what swapping the state under a deployed model would do. Details, per-family errors and the eviction numbers are in `results/state_shootout/`.
**Result.** Apple M3 Pro, go1.26.5. **Timings were taken on a shared, heavily loaded machine (1-minute load average 12 to 40) and varied 30% between repetitions. Re-run on a quiet machine before quoting.**

| State | Bytes per key | ns per update | ns per read | Features exact (share of rows) | Worst feature error | Fraud recall at 1% FPR |
|---|---|---|---|---|---|---|
| Exact | 380 | 349 | about 0 beyond the update | 1 (reference) | 0 (reference) | 22.49% (ROC-AUC 0.8450, PR-AUC 0.2983) |
| Bucketed ring | 613 | 320 | about 0 beyond the update | 76% of counts and sums, 45% of distinct counts, 100% of recency | sums over by up to $77,000 (one trailing bucket); counts never under | 22.49% (ROC-AUC 0.8450, PR-AUC 0.2982) |
| Sketch | 69 MB fixed (1,440 per live key here) | not re-measured (see below) | not re-measured | 62% of counts and sums, 2.5% of distinct counts, 99.999% of recency | sums over by up to $232,000; 18 recency values missing where exact has them, none the other way | 22.21% (ROC-AUC 0.8427, PR-AUC 0.2938) |
| Sketch, before the recency fix | 64 MB fixed (1,331 per live key here) | 462 | about 940 | 62% of counts and sums, 2.5% of distinct counts, 88% of recency | recency: an unseen key read as seen on 392,560 feature values; `uid_seconds_since_first` exact on 23.5% of rows | 22.77% (ROC-AUC 0.8314, PR-AUC 0.2867) |

"ns per read" is ReadAdd minus Add, because scoring always reads a key right after its previous update. For exact and bucketed the difference is inside the noise. At this data's scale the exact state is also the smallest: 48,245 live keys and 18 MB at the end, 28 MB at the peak. Idle-key eviction cuts exact from 74 MB to 18 MB and bucketed from 126 MB to 30 MB, with every feature bit-identical with and without it.

The bucketed ring costs nothing measurable on the metrics. It changes a third of validation scores slightly, and ROC-AUC and PR-AUC move in the fifth decimal.

The first run found a bug in the sketch. First and last seen were min/max sketches shared by all keys, and a key counted as seen when all its cells held a time. Once other keys had touched every cell, every new card or customer read as seen, with another key's first-seen time. First and last seen now come from a fixed-size table of full 64-bit key hashes (6.3 MB, 8-way sets). Its only error is forgetting a live key when a set overflows, which makes the key look newer, never older. On this data that happened on 9 payments in six months. The log is [`internal/features/BUGLOG.md`](internal/features/BUGLOG.md). Before the fix the sketch cost 0.0137 ROC-AUC and 0.0116 PR-AUC. After it, the sketch costs 0.0024 ROC-AUC and 0.0044 PR-AUC, and the rest comes from count-min and HyperLogLog error. Its recall at 1% FPR moved from 8 frauds above exact to 8 below, out of 2,850, which is noise at one operating point. The re-run was on an even busier machine (load average 44 to 57), where the unchanged exact state timed 3.5 times slower than before, so the sketch's speed after the fix is not reported. Re-measure it on a quiet machine. The exact row reproduces `python/train.py`'s validation numbers for the same model to about 1e-13. The validation month was the model's early-stopping set, so the absolute values are optimistic for all three states alike.

### 5. Latency under load

**Question.** What is the highest arrival rate at which p99 stays inside the deadline?
**Method.** `scripts/experiments/exp5.sh` runs `cmd/loadgen` open loop at fixed rates, measuring latency from each request's intended send time (see `docs/LOADGEN.md`) with HdrHistogram percentiles. The requests are the test month's 92,427 real IEEE-CIS payments, labels stripped, sent round-robin with a unique `payment_id` on every send. Each configuration gets a fresh `riskgate serve` with models/ieee (954 trees), `rules/default.rules` and exact state. It sweeps shard counts 1, 4, 16 and 64 against a single mutex and `sync.Map`. Every rate runs 5 s of warmup and 20 s of measurement. Rates climb from 500 until two in a row miss the deadline, and the whole sweep ran twice. A rate counts as inside only if p99 is at most 50 ms with no errors, timeouts or non-2xx responses. The in-process cost comes from the `internal/service` benchmarks with `RISKGATE_BENCH_MODEL` and `RISKGATE_BENCH_REQUESTS` set.
**Result.** Machine: Apple M3 Pro (6P+6E cores, 12 logical CPUs), go1.26.5, GOMAXPROCS 12 for both server and load generator, on the same machine. Deadline 50 ms. Run 2026-09-23. **These numbers are not quotable.** The machine was shared with a VM, Docker and other experiments. The 1-minute load average before each rate ranged from 10.5 to 66 (median 38.8) on 12 CPUs, and in 52 of 58 rate steps the load generator itself fell more than 1 ms behind schedule at p99. Load changed while the configurations ran one after another, so the ranking between configurations below reflects machine noise, not the data structures. Re-run `scripts/experiments/exp5.sh` on a quiet machine before quoting any number here.

| State concurrency | Rate (req/s) | p50 | p99 | p99.9 | p99 inside deadline | Highest rate inside, per run |
|---|---|---|---|---|---|---|
| Single mutex | 1000 | 1.75 ms | 49.5 ms | 88.6 ms | 1 of 2 runs | 1000, 500 |
| `sync.Map` | 1000 | 1.76 ms | 36.8 ms | 66.7 ms | 1 of 2 runs | none, 1000 |
| Sharded, N = 1 | 1000 | 1.00 ms | 23.7 ms | 89.7 ms | 2 of 2 runs | 8000, 1000 |
| Sharded, N = 4 | 1000 | 1.13 ms | 24.0 ms | 47.5 ms | 2 of 2 runs | 5000, 1000 |
| Sharded, N = 16 | 1000 | 1.05 ms | 6.8 ms | 23.0 ms | 2 of 2 runs | 3000, 3000 |
| Sharded, N = 64 | 1000 | 1.36 ms | 50.5 ms | 110.1 ms | 1 of 2 runs | none, 1000 |

Highest rate with p99 inside the deadline: not established. On this loaded machine the best single run reached 8,000 req/s (sharded, N = 1), but the same configuration's second run failed at 2,000. Full per-rate tables are in `results/exp5/exp5.md`.

Single-request cost in process, median of 5 runs, taken while the load average was 78 to 100 (so these are upper bounds). The full pipeline (features, model, Saabas contributions and rules) costs 189 µs. Within it, the model score takes 78 µs and the Saabas contributions for reasons take 88 µs. The handler, which adds JSON and the decision log, costs 286 µs, and a request over loopback HTTP costs 473 µs. The 954-tree model is nearly all of the pipeline's cost. In this run the contributions took a second walk of the trees, which nearly doubled it. The pipeline now gets the score and the contributions from one walk (`Scorer.ScoreContributions`), which costs about what the contributions alone did.

### 6. Backtest speed

**Question.** Is a backtest fast enough that an analyst iterates?
**Method.** `backtest bench` (`backtest.RunBench`). One rule and the 50-rule `RealisticRuleSet` over all 590,540 rows of the real table, scored by the trained model. Vectorized is compared with row-at-a-time on the same cached features, on one thread and on GOMAXPROCS=12. Row-at-a-time gets a row store built in advance for free, which flatters it. Vectorized also produces every rule's match bitmap, which the backtester needs for overlaps and row-at-a-time does not. The page's case is `Backtester.Run`, one proposed rule against the cached 50-rule baseline. Each case ran 9 times in each of 3 runs; the table gives the best time, and `results/backtest_speed/` has medians and the export-table runs. Every run checked that the two evaluators' decisions were identical on all rows.
**Result.** Apple M3 Pro (6P+6E), go1.26.5, GOMAXPROCS=12. **The machine was shared and heavily loaded (1-minute load average 13 to 46), and identical code varied by up to 20% between runs. Re-run on a quiet machine before quoting.**

| Workload | Evaluator | Threads | Best time | Rows per second |
|---|---|---|---|---|
| One rule | Vectorized | 1 / 12 | 1.83 ms / 0.47 ms | 323M / 1.26G |
| One rule | Row-at-a-time | 1 / 12 | 29.1 ms / 9.37 ms | 20.3M / 63.0M |
| 50-rule set | Vectorized | 1 / 12 | 32.6 ms / 4.83 ms | 18.1M / 122M |
| 50-rule set | Row-at-a-time | 1 / 12 | 57.6 ms / 9.58 ms | 10.3M / 61.6M |
| Page: one proposed rule against the cached baseline | Vectorized | 12 | 0.58 ms (3.6 ms the first time a period is used) | |

At first the vectorized 50-rule set was only 1.6x faster than row-at-a-time on one thread (47.6 against 78 ms on the exported table). Timing each rule showed why: the three numeric `in` rules each cost several times as much as the other rules, because their kernel branched on the data. Three changes followed. Numeric `in` became branch-free (a word-at-a-time scan for up to 4 values, a collision-free hash table beyond that). Radar's order is now decided per chunk inside the parallel workers instead of in one sequential pass afterwards. The backtester keeps the last resolved backtest period instead of rebuilding it on every run. The 50-rule set went from 48.2 to 32.6 ms on one thread and from 8.1 to 4.8 ms on 12, and the page's run from 3.75 to 0.58 ms (scored table, best times). One tried idea did not pay off: skipping rows an earlier action already decided saved 3%, because about 22% of rows are still undecided after the block rules, which is too dense for row-at-a-time refinement to win. It was removed.

### 7. Label delay (simulated)

**Question.** How far does a naive recent-window backtest understate a rule's fraud catch, and does the maturity window fix it?
**Method.** Simulate a dispute arrival time for each fraud label from `DefaultDelay` (lognormal, median 30 days, sigma 0.5, capped at 120 days, an assumption and not a measurement). Compare a naive last-30-days backtest and a matured one against the truth (`Backtester.CompareLabelDelay`).
"Today" is the end of the validation month, so the naive window is the validation month and the matured window (30 days ending 60 days earlier) falls in the training months. No test-month payment is used. Rule: `review if :product_code: = "C" and :amount: > 50`, from `rules/baseline.rules`, backtested with no rules in force on the real table. Three more rules, and the sensitivity to the assumed delay, are in `results/label_delay/`.
**Result.** Every number here is SIMULATED.

| Backtest | Fraud caught as reported | Fraud caught in truth | Understatement |
|---|---|---|---|
| Naive, last 30 days | 86 payments, $7,736 | 461 payments, $42,447 | 81.8% of fraud dollars |
| Matured, 60-day maturity | 316 payments, $27,133 | 325 payments, $28,164 | 3.7% of fraud dollars |

Across the four rules tried, the naive backtest understates fraud dollars caught by 80% to 88% and precision by a factor of 5 to 8. The 60-day window brings the understatement down to 4% to 6%. The size of the error depends on the assumption. With a 15-day median delay the naive understatement is 47%. With 45 days it is 95%, and 60 days of maturity still leaves 17%.

### 8. End to end through Clearinghouse

**Question.** What does the whole system do to losses on real transaction amounts and real labels?
**Method.** Replay the test month through Clearinghouse's API, with Clearinghouse calling RiskGate on every confirm. Fraud-labelled payments use Clearinghouse's disputing test card, so disputes arrive in simulated time and flow back as labels by webhook. Compare three policies. Everything around the data is a simulation driven by it, and the per-dispute fee is an assumption.
**Result.**

Run by Clearinghouse's `bin/exp-replay` on 2026-09-25 over all 92,427 test-month payments. Each policy used its own RiskGate, started from the warm snapshot (velocity state as of the start of the test month), and the model hash was checked before each run. Details and the source files are in [`results/exp8/README.md`](results/exp8/README.md).

| Policy | Disputes | Dispute losses (amount plus assumed fee) | Legitimate revenue blocked | Payouts held for review | Net of checks |
|---|---|---|---|---|---|
| No risk checks | 3,213 | $535,657.76 | $0 | 0 | $0 |
| Model threshold alone (`risk_score >= 50`) | 2,986 | $472,390.72 | $40,359.44 (157 payments) | 0 | +$22,907.60 |
| Model plus rules (`rules/default.rules`) | 2,986 | $472,390.72 | $40,359.44 (157 payments) | 2,496 | +$22,907.60 |

Assumed per-dispute fee: $15.00, Stripe's published US dispute fee, used as an assumption in both RiskGate's and Clearinghouse's write-ups.

Dispute losses fell 11.8%, and net of the legitimate revenue blocked the checks came out $22,907.60 ahead over the month. The block rule caught 227 frauds out of 384 blocked payments, 59% precision against the 71% it had on validation, where its threshold was chosen. The two model policies have identical money outcomes because both block on the same rule. The rule set's other rules are review rules, and in Clearinghouse a review lets the payment through and holds the merchant's payout, so they change who waits for money, not how much is lost. Nobody works the review queue in this simulation. That makes the honest summary of the review rules "2,496 holds opened". What those holds are worth depends on a reviewer this simulation does not have.

The run also closes the loop the spec describes. Clearinghouse delivered all 2,986 dispute events and 92,043 success events to RiskGate's webhook receiver as signed webhooks, and every delivery succeeded. RiskGate recorded them as labels, which is the path by which a production RiskGate would learn from its own mistakes. The latency measured inside these runs is not quotable, because the runs were concurrent on a loaded machine that also slept mid-run.

### 9. The risk service fails

**Question.** What does failing open actually cost?
**Method.** Clearinghouse's failure runner (`bin/exp-risk-failure`) sends an open-loop stream of risk checks at 200/s through Clearinghouse's real RiskGate client: a 50 ms deadline, a circuit breaker that opens after 5 consecutive failures with a 2 s cool-down, and fail-open. RiskGate runs the IEEE-CIS model and `rules/default.rules` from the warm snapshot (velocity state as of the start of the test month). Each run is 10 s healthy, 10 s down, 10 s healthy. In **kill** mode RiskGate is SIGKILLed and a new process starts over its last periodic snapshot, where connections are refused. In **freeze** mode it is SIGSTOPped and resumed, where connections hang, which is the case the deadline exists for. `scripts/experiments/exp9.sh`; full write-up in [`results/exp9/README.md`](results/exp9/README.md).
**Result.**

| Mode | Calls | Went through unchecked | First checked payment after recovery | Client p99, whole run | Client max |
|---|---|---|---|---|---|
| Kill (process restarts from snapshot) | 6,000 | 3,240 | 2.06 s after restart | 7.4 ms | 57.3 ms |
| Freeze (connections hang) | 6,000 | 2,063 | 0.32 s after thaw | 5.5 ms | 59.0 ms |

Restart to ready over the 4.16 MB warm snapshot (498,113 payments of history) took 383 ms. The cost of failing open is simple to state: every payment during the outage goes through unchecked, about 2,000 at 200/s over 10 s. In kill mode that also includes the breaker's 2 s cool-down after RiskGate is back, which is why the first checked payment comes 2.06 s after a restart that was ready in 0.38 s. Clearinghouse's latency stayed bounded by the deadline throughout, with a maximum of 57 to 59 ms against 50 ms plus overhead, so payments kept flowing. In freeze mode only the first 16 calls paid the full deadline before the breaker opened.

Two things are not settled. First, in kill mode the breaker also opened once in each healthy phase, about 6 to 7 s after each process started, costing about 415 unchecked payments each time. It reproduced in two runs and never happened in freeze mode. Periodic snapshots and GC were ruled out directly. The machine was not idle during either run, so contention is the leading explanation, but a cold-start effect has not been ruled out. Those numbers are not cited until an idle rerun. Second, how many of the unchecked payments were fraud depends on which payments land in the outage window. The fraud count and its dollar cost come from experiment 8's replay, which fails RiskGate the same way.

## Bug log

Real bugs found by tests, fuzzing and parity checks, each with a regression test.

- [`internal/rules/BUGLOG.md`](internal/rules/BUGLOG.md). Two parser bugs found by `FuzzParse`. The checker was quadratic in nesting depth (a 20,000-deep `is_missing` chain took 7.0 s to load and now takes 58 ms), and junk input produced one diagnostic per byte.
- [`internal/backtest/BUGLOG.md`](internal/backtest/BUGLOG.md), for the vectorized evaluator and the differential test. The differential test never failed on its own, so the log also records the planted bugs used to prove it can fail, including a planted two-valued `not`.
- [`internal/features/BUGLOG.md`](internal/features/BUGLOG.md). The sketch's first- and last-seen tracking reported never-seen keys as seen (392,560 wrong values over the dataset), found by experiment 4's per-feature error report. It cost 0.0137 validation ROC-AUC before the fix and 0.0024 after.
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
