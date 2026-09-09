# Experiment 2: train/serve parity

Command `scripts/experiments/exp2.sh`, git commit `2e59251316e7bfe67102862b9f942e05896214a4`, 2026-09-23T23:33:10Z to 2026-09-23T23:40:41Z.

Machine: Apple M3 Pro (6P+6E cores, 12 logical CPUs), go1.26.5, GOMAXPROCS default (12).
Load average (1/5/15 min): start 20.56 27.84 41.46; after the service replay 12.13 22.35 36.41; end 15.71 20.22 32.01. The machine was shared.
Parity results do not depend on load; only the replay's wall time does.

Data: real IEEE-CIS transactions (all 590,540, test month = month 5), export `data/export_real` (export.csv sha256 `91fac348dc4abb1c...`),
model `models/ieee` (954 trees, model.txt sha256 `787a2e4e548406a0...`). No step reads labels.

## a) Go evaluator against LightGBM, every test-month row

| Rows checked | Bit-identical | Max abs diff |
|---|---|---|
| 92,427 | 92,427 | 0 |

## b) Service features against the offline export, every row

A fresh `riskgate serve` (no snapshot, exact state, 64 shards, rules/default.rules) received all 590,540 transactions
sequentially in (TransactionDT, TransactionID) order over HTTP (sent 590540 requests sequentially in 3m2.824s (3230 req/s), every response 200).
Each decision-log line was compared with a fresh offline replay (all 53 catalog fields, bits),
with export.csv (all 53 encoded model inputs, bits), and with the offline Scorer (raw score bits, probability bits, risk_score).
Missing on both sides counts as equal.

Decision log: {'riskgate_decision_log_dropped_total': '0', 'riskgate_decision_log_write_errors_total': '0', 'riskgate_decision_log_written_total': '590540'}. Log lines 590,540; offline rows missing from the log 0; unmatched log lines 0.

| Rows | Compared | Feature rows differ | Encoded vs export.csv differ | Raw score differ | Probability differ | risk_score differ | Max abs feature diff | Max abs raw diff |
|---|---|---|---|---|---|---|---|---|
| All months | 590,540 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| Test month | 92,427 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |

Field-level mismatches: none.

## c) `riskgate audit` on the decision log

590,540 decisions, 590,540 replayed identically, 0 mismatched, rule-set versions {'1': 590540}.

Verdict: PASS, zero mismatches in every check.

## Negative control

To show the check can fail, a copy of the decision log had the first row's amount moved by one ulp
(68.5 to 68.50000000000001) and one line deleted. `serveparity compare` on the copy reported
1 differing feature row (fields amount), 1 differing encoded row, max abs feature diff 1.4210854715202004e-14, 1 row missing from the log,
exit code 1. Both faults detected: yes.
