# Rules-only baseline: tuning log

The "rules alone" row of experiment 1. The rule set is `rules/baseline.rules`: 8 hand-written rules (1 allow, 1 block, 6 review) over raw fields and velocity features. None uses `:risk_score:`.

- **Data.** IEEE-CIS (real), `data/export_real/features.table` (590,540 rows, built by `cmd/export` with the exact velocity state, `risk_score` NaN).
- **Tuning rows.** The validation month only, month 4 of `internal/data/split.go`: 83,571 payments, 2,850 fraud (3.41%). The test month was not read, printed or scored while tuning.
- **Tools.** Exploration with pandas on the `split == "valid"` rows of `data/export_real/export.csv` (fraud rate by category, by quantile of each velocity feature, and for about 60 two- and three-way combinations). Rule evaluation with the Go rule engine: `backtest rulestats -split valid`, added for this (`cmd/backtest/flag.go`). It prints each rule's standalone matches, precision, recall and FPR, how many rows each rule decides in Radar order, and the set's totals.
- **Flagged.** A payment counts as flagged when its final decision in Radar order is block or review. A payment an allow rule matches is not flagged.
- **Provenance.** `provenance.json`: commit 2e59251 plus this session's uncommitted changes, 2026-09-23, Apple M3 Pro (6P+6E), go1.26.5. Nothing here is a timing, so machine load does not matter. `validation.json` and `train.json` are `backtest rulestats -split valid|train -json` output, aggregates only.

The thresholds were picked by looking at validation, so the validation numbers below are optimistic. The test-month number, computed once by the lead, is the honest one.

## What the validation month showed

- Product C is 10% of payments and 38% of fraud (12.4% fraud rate). Almost every product C payment has no `billing_region` (addr1). The approximate `uid` needs addr1, so **uid velocity features are missing for nearly all product C payments**, which is where fraud concentrates.
- Credit is 7.5% fraud and debit 2.3%. Discover is 12.7%. Recipient domain gmail.com is 14.3% (19.8% within product C), and outlook.com is 20% but only 330 payments.
- Most velocity features are weak on their own. High card counts are *less* fraudulent than average (`card_txn_count_24h` above its 99th percentile: 0.5% fraud), because `card1` is coarse and busy `card1` values are shared by many customers. Device counts are over `device_info` model strings ("Windows"), and email counts are over whole domains, so both mostly measure traffic. Within product C, velocity helps: a repeat on the same card within 3 minutes (17.7%) and a device model with more than 12 payments in the hour (17.0%).
- `anonymous.com` as purchaser is *below* the base rate (2.0%), so the textbook "anonymous.com and amount > 300" rule from `rules/default.rules` gets 1.3% precision here and was dropped.

## Iterations (validation month; set-level numbers)

| Version | Rules | Flagged | Precision | Recall | FPR | Fraud-dollar recall | What changed |
|---|---|---|---|---|---|---|---|
| r0 | 3 | 14,608 | 9.2% | 47.3% | 16.4% | 22.4% | Starting point: anonymous.com > $300, distinct cards per device > 5, product C. Far too broad. |
| v1 | 10 | 6,999 | 14.5% | 35.5% | 7.42% | 34.8% | First analyst-style set. Its allow rule let through 2.8% fraud, close to the base rate. |
| v2 | 9 | 8,292 | 13.4% | 39.1% | 8.89% | 20.7% | Stricter allow (uid first seen > 90 days, last > 7 days: 0.8% fraud). Broad "product C and credit" review added too much volume. |
| v3 | 8 | 5,709 | 16.1% | 32.2% | 5.94% | 15.6% | Dropped broad reviews, added product-C velocity rules. |
| v4 | 10 | 7,023 | 15.5% | 38.2% | 7.35% | 34.6% | Added a product W credit uid-spend rule and a Discover rule, which recover fraud dollars (C is low-dollar). |
| v5 | 9 | 5,012 | 17.8% | 31.3% | 5.10% | 30.6% | Dropped the broad gmail/outlook recipient rule (its marginal rows were 9.4% fraud) and a device rule that decided 7 fraud. |
| **v6 = baseline.rules** | **8** | **4,803** | **18.4%** | **31.0%** | **4.86%** | **30.6%** | Dropped the outlook.com rule (10 marginal fraud in 209 rows). |

Rejected candidates, each on its own on validation: "product C and card amount ratio > 2" (21.0%, but only 7 fraud not already caught), "uid 7-day count >= 8" (8.7%), "credit and mobile" (14.3%, 30 marginal fraud in 560), "W, amount > 400, card ratio > 3" (3.8%), "W, uid ratio > 3, amount > 200" (1.7%), "distance > 1000 and credit" (5.0%), "card seen again within 60 s and amount > 200" (8.4%).

The allow rule barely changes the set. Without it, 6 more legitimate payments are flagged and nothing else changes. It stays because a real set has one, and it exercises Radar's order.

## Final numbers

Per rule (validation, standalone):

| Rule | Matches | Precision | Recall | Decides (fraud) |
|---|---|---|---|---|
| allow: uid first seen > 90 d and last > 7 d | 3,724 | 0.8% | 1.0% | 3,724 (28) |
| block: C, credit, recipient gmail.com, > $30 | 788 | 35.0% | 9.7% | 788 (276) |
| review: billing_country_code != 87 | 413 | 13.8% | 2.0% | 372 (49) |
| review: C and amount > $50 | 2,171 | 21.2% | 16.2% | 1,665 (284) |
| review: C and card seen again within 180 s | 858 | 17.7% | 5.3% | 586 (80) |
| review: C and device model > 12 payments in 1 h | 493 | 17.0% | 3.0% | 273 (31) |
| review: W, credit, uid 24 h spend > $500 | 648 | 14.8% | 3.4% | 648 (96) |
| review: Discover and amount > $100 | 543 | 17.1% | 3.3% | 471 (67) |

The set (`validation.json`) on validation: 4,803 flagged of 83,571, 883 of them fraud. Precision 18.4%, fraud recall 31.0%, FPR 4.86%, fraud-dollar recall 30.5%, legitimate dollars flagged 7.0%.

For comparison, the same rules on the training months (`train.json`, not used for tuning) give precision 12.4%, recall 27.8%, FPR 7.18% and fraud-dollar recall 23.5%. Rules picked by eye on one month transfer imperfectly to others. Expect the test month to fall below validation.

Note for experiment 1's table. A rule set is one operating point (FPR 4.86% on validation), not a ranking. Its "recall at 1% FPR" is undefined unless the metric code interpolates, and it has no ROC curve.

## Predictions file

```
go run ./cmd/backtest flag -table data/export_real/features.table -rules rules/baseline.rules -out data/rules_predictions_real.csv
```

This writes TransactionID,flagged for all 590,540 rows (43,603 flagged), in the gitignored `data/` directory. SHA-256 at writing: `62800cad4846bf4765d5b7d57ef9fccedd334f0e702126ea89bbd5f511be7d3a`. The rules file's SHA-256 is `691a57e435ef5a1af10682cb75a25910f9106e53eabd9c3aebfdab844dcf5c76`. A pandas recomputation from the CSV on the validation rows matched the Go numbers exactly (4,803 flagged, precision 0.18384, recall 0.30982).
