# Experiment 2 bug log

Run of 2026-09-23 (`scripts/experiments/exp2.sh`, results in `exp2.md`).

**No parity bugs found.** Every check came back with zero mismatches:

- Service feature vectors against a fresh offline replay: 590,540 rows, 0 differ (test month: 92,427 rows, 0 differ).
- Service feature vectors, encoded, against `export.csv`: 0 differ.
- Service raw score, probability and `risk_score` against the offline Scorer: 0 differ.
- Go evaluator against LightGBM on the test month: 92,427 of 92,427 bit-identical.
- `riskgate audit`: 590,540 decisions, all replayed identically.

The usual suspects were covered by construction and confirmed by the data. The amount travels as `risk_fields.TransactionAmt`, and a
float64 survives the JSON round trip, so the rounded cents are never used when TransactionAmt is present. D1 and other missing
values are left out of the payload and parse back as NaN. Event time is `created - ReplayEpochUnix`, which is exact integer
arithmetic. Same-second ties are ordered by TransactionID on both sides.

The negative control (`negative_control.json`) confirms the check can fail: a one-ulp change to one amount and one deleted log line
were both reported.

Not bugs, but noted while running:

- `riskgate serve` refuses to start without a webhook signing secret, even when no webhooks will arrive. The experiment scripts pass
  a random throwaway secret through `RISKGATE_WEBHOOK_SECRETS`.
- The decision log escapes invalid UTF-8 as U+FFFD, the same way `encoding/json` does. A raw string field with invalid UTF-8 would
  therefore log differently from the value the engine saw. No IEEE-CIS string in this run triggered it: the string fields compared
  equal on every row.
