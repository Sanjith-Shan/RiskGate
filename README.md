# RiskGate: Real-Time Fraud Rules Engine

RiskGate is a fraud rules engine in Go that sits in a payment's critical path and answers allow, block, or review. Analysts write rules in a small typed language with error messages meant for people who are not programmers, and a backtester replays 590K real e-commerce transactions to show what a rule would have caught and what it would have cost before it goes live. A gradient-boosted model, evaluated natively in Go, feeds the rules a calibrated `risk_score`. It does not make the decision.

The design is in [DESIGN.md](DESIGN.md). It starts with what the data can and cannot say.

## Results

Nothing here is filled in until it is measured. Each cell links to the experiment that produces it.

| What | Result | Where |
|---|---|---|
| Fraud recall at 1% FPR, raw columns to raw plus velocity features (test month, time split) | TBD (experiment 1) | [Experiment 1](DESIGN.md#1-what-the-streaming-features-are-worth) |
| Train/serve parity, service features against offline export | TBD (experiment 2) | [Experiment 2](DESIGN.md#2-trainserve-parity) |
| Go evaluator against LightGBM raw scores, rows bit-identical | TBD (experiment 2) | [Experiment 2](DESIGN.md#2-trainserve-parity) |
| Closure and vectorized evaluators, generated rules and disagreements | TBD (experiment 3) | [Experiment 3](DESIGN.md#3-two-evaluators-one-answer) |
| Highest rate with p99 inside the deadline, on a labelled machine | TBD (experiment 5) | [Experiment 5](DESIGN.md#5-latency-under-load) |
| Backtest of one rule over 590K rows | TBD (experiment 6) | [Experiment 6](DESIGN.md#6-backtest-speed) |
| Simulated dispute losses, no checks against model plus rules, through Clearinghouse | TBD (experiment 8) | [Experiment 8](DESIGN.md#8-end-to-end-through-clearinghouse) |

RiskGate does not compare itself with Stripe Radar or with the Kaggle leaderboard. Neither comparison would be fair or meaningful. The claim is the ablation in experiment 1 and the correctness results in experiments 2 and 3.

## Demo

`TBD: GIF of typing a rule into the page, seeing an error with a caret, fixing it, and reading the backtest. It will be recorded on synthetic data, since the real data cannot be republished.`

## A rule, and what RiskGate says about it

```
allow  if :purchaser_email_domain: in @trusted_domains
block  if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:
block  if :risk_score: >= 85
review if :distinct_cards_per_device_24h: > 3
review if :product_code: = "C" and is_missing(:device_info:)
```

Evaluation follows the order Stripe documents for Radar. Allow rules run first and win, then block, then review. A mistake gets an error with a caret and a suggestion.

```
error at line 1, column 10:
block if :card_txn_cnt_1h: >= 8
         ^^^^^^^^^^^^^^^^^
unknown attribute :card_txn_cnt_1h:. Did you mean :card_txn_count_1h:?
```

## Quickstart, on synthetic data

This runs without a Kaggle account. `cmd/synth` writes a seeded dataset in the IEEE-CIS file format, and every tool that reads it prints `SYNTHETIC`. **Numbers from synthetic data are about the code, never about payments.**

Requires Go 1.26 and, for training, Python 3.12.

```sh
make synth                                        # 590,540 synthetic rows into data/synth
make export EXPORT_ARGS="-synthetic"              # features via the Go engine into data/export_synthetic

# check a rule file the way the service will before swapping it in
go run ./cmd/rulecheck path/to/rules.txt

# backtest one rule over a generated table
go run ./cmd/backtest run -table synthetic \
  -rule 'block if :purchaser_email_domain: in @risky_domains and :amount: > 300'

# the risk_score threshold sweep, and the two-evaluator differential test
go run ./cmd/backtest sweep -table synthetic
go run ./cmd/backtest difftest -rules 5000 -table synthetic
```

Training runs offline in Python on the Go export.

```sh
python3 -m venv .venv && . .venv/bin/activate
pip install -r python/requirements.txt
make train TRAIN_ARGS="--export data/export_synthetic --source synthetic --out models/synthetic"
```

> **Not working yet.** `python/train.py`, `evaluate.py` and `parity.py` read `export.csv`, and `cmd/export` currently writes `features.csv`, so training and the parity check fail on a fresh export until the two agree. The commands below use the name the Python scripts expect.

The service is not built yet. When it is, this is the command, and it does not exist today.

```sh
go run ./cmd/riskgate ...                         # NOT YET IMPLEMENTED: /v1/assess, /v1/rules, the page
```

## Using the real data

The IEEE-CIS data belongs to the competition and may be used for non-commercial research and education. It may not be redistributed, so this repo never contains it or anything row-level derived from it (see [Data license](DESIGN.md#data-license)). Get it yourself.

1. Sign in to Kaggle and accept the rules at https://www.kaggle.com/competitions/ieee-fraud-detection/rules. Downloads are refused until you do.
2. Create an API token under Kaggle **Settings > API**. Recent Kaggle CLIs read `~/.kaggle/access_token`, and older ones read `~/.kaggle/kaggle.json`. `scripts/fetch_data.sh` currently checks for `kaggle.json`, or `KAGGLE_USERNAME` and `KAGGLE_KEY` in the environment.
3. Download and build the features.

```sh
scripts/fetch_data.sh                             # train_transaction.csv and train_identity.csv into data/raw
make export                                       # first run builds data/cache/ieee.rgc, then writes data/export
make train TRAIN_ARGS="--export data/export --source ieee-cis --out models/current"
python3 python/evaluate.py --export data/export --model-dir models/current   # the test month, once
```

Train/serve parity on every test-month row.

```sh
python3 python/parity.py --export data/export --model-dir models/current --out results/parity_scores.csv
go run ./cmd/parity -model models/current -export data/export/export.csv -scores results/parity_scores.csv
```

Everything under `data/`, `models/` built from real data, and decision logs stay on your machine.

## Repository layout

```
cmd/
  synth/        synthetic IEEE-CIS-shaped dataset
  export/       offline feature export, the same feature code the service runs
  parity/       Go model scores against LightGBM on every row
  rulecheck/    validate a rule file, errors with carets
  backtest/     backtests, the threshold sweep, experiments 3, 6 and 7
  loadgen/      open-loop load generator for experiment 5 (docs/LOADGEN.md)
internal/
  schema/       the field catalog shared by features, rules, model and backtester
  data/         loader, (DT, ID) ordering, month split, cache, replay format
  features/     feature engine, velocity state (exact, bucketed, sketch), snapshots
  model/        LightGBM text-model evaluator, calibration, Saabas reasons
  rules/        lexer, Pratt parser, checker, linter, closure compiler, generator
  backtest/     columnar table, vectorized evaluator, reports, sweep, label delay
  webhook/      Clearinghouse signature verifier, dedupe, event handler
  loadgen/      load generator internals
  service/      the online service (in progress)
python/         offline training, baselines, evaluation, parity, fixtures
scripts/        data download, fuzzing, signature-vector checks
docs/           load generator notes, interview preparation
```

## Testing

```sh
make race        # every test under the race detector, which is what CI runs
make lint        # go vet and staticcheck
make fuzz        # every native fuzz target, FUZZTIME each (default 20s)
make cover       # race tests with coverage
make bench       # Go benchmarks
```

What the tests are there to prove.

- **Point in time.** `TestLeakage` rewrites everything after a sampled payment, reshuffles the input, replays it, and requires that payment's features to be bit-identical. It runs against all three velocity states. [More](DESIGN.md#the-leakage-test)
- **Two evaluators, one answer.** `TestDifferential` checks the closure evaluator against the vectorized one on generated, well-typed rules over tables seeded with adversarial values. [More](DESIGN.md#two-evaluators-and-the-differential-test)
- **Model parity.** `TestParityWithLightGBM` requires bit-identical raw scores on fixtures that probe every split threshold, one ulp either side, signed zeros, LightGBM's zero threshold, NaN and infinities. `cmd/parity` does the same on real data. [More](DESIGN.md#the-go-evaluator)
- **Parser fuzzing.** `FuzzParse` checks no panics, a print and parse round trip, and agreement with a reference interpreter. Bugs it found are in [internal/rules/BUGLOG.md](internal/rules/BUGLOG.md).
- **Error messages.** 35 golden files under `internal/rules/testdata/errors`.
- **Webhook signatures.** Shared vectors with Clearinghouse's Ruby verifier, and CI re-checks RiskGate's vectors against an independent Python and `openssl` implementation. [More](DESIGN.md#signatures)
- **Snapshots.** Restored velocity state is byte-identical to the state that was saved.

## Design notes

- [What the data can and cannot say](DESIGN.md#what-the-data-can-and-cannot-say)
- [One feature implementation, and point-in-time correctness](DESIGN.md#one-feature-implementation-and-point-in-time-correctness)
- [Velocity state: exact, bucketed ring, sketch](DESIGN.md#velocity-state)
- [Why the model feeds the rules](DESIGN.md#why-the-model-feeds-rules-instead-of-deciding), and [what `risk_score` means](DESIGN.md#calibration-and-what-risk_score-means)
- [Missing values and three-valued logic](DESIGN.md#types-and-missing-values), matching Radar and SQL `WHERE`
- [The backtester and label maturity](DESIGN.md#the-backtester)
- [Fail open, and what it costs](DESIGN.md#fail-open-and-what-it-costs)
- [Reading list and credits](DESIGN.md#reading-list-and-credits)

RiskGate's sibling is Clearinghouse, a payments ledger that calls RiskGate on every confirm and reports disputes back by webhook.

## License

MIT, see [LICENSE](LICENSE). The license covers the code. The IEEE-CIS data is under the competition's own rules and is not part of this repository.
