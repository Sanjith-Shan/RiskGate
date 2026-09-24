# Production rule set: tuning log

The rule set the service runs (`rules/default.rules`, experiment 8's "model plus rules") and the model threshold alone (`rules/model_only.rules`, experiment 8's "model threshold alone"). Both use `:risk_score:` from the trained model `models/ieee`.

- **Data.** IEEE-CIS (real), `data/cache/table_scored_real.rgt`, the backtest table with `risk_score` filled by `models/ieee`.
- **Tuning rows.** The validation month only (month 4, days 121 to 151 after the epoch, April 2018 on the assumed calendar): 83,571 payments, 2,850 fraud (3.41%), $462,548 fraud dollars and $10,710,804 legitimate dollars. The test month had already been evaluated once and was closed. It was not read, printed or scored here.
- **Tools.** `backtest sweep` for the threshold. `backtest run -current <set> -rule <rule>` for each rule's incremental effect, meaning the payments whose decision the rule changes given the other rules. `backtest rulestats -split valid` for the set totals. The backtest tool got a `-lists` flag for this work (every subcommand that reads a table), so rules are checked against the service's `rules/lists.json` instead of the built-in sample lists (which, for example, have no `@disposable_domains`).
- **Label maturity.** Off (`-maturity-days 0`). IEEE-CIS labels are final, and the window is historical.
- **Cost accounting.** The same as experiment 8. A blocked legitimate payment loses its amount. A fraud that gets through costs its amount plus an assumed $15 dispute fee. "Net" below is fraud dollars + $15 per fraud − legitimate dollars, over the payments a rule changes. In Clearinghouse, review lets the payment proceed and holds the merchant's payout (`risk-integration.md`). So review costs analyst time, not the sale, and it is judged on precision, not on net.
- **Honesty caveats.** `risk_score`'s isotonic calibration was fitted on this month, and the model early-stopped on it. The rules were then picked by looking at it. Every number here is optimistic, and the test month is the honest measurement. Only aggregates appear here.

## Step 1: the block threshold, from the sweep

`risk_score` is a calibrated probability in percent. Blocking a payment of amount *a* pays when *p*(*a* + 15) > (1 − *p*)*a*, i.e. *p* > *a*/(2*a* + 15), which is just under 50%. The validation scores sit on the isotonic calibration's steps. So the sweep's rows only change at a step, and it is clearer read step by step (`sweep_valid.json`):

| Score step | Payments | Fraud | Precision | Net of the step |
|---|---|---|---|---|
| 25 | 173 | 44 | 25% | −$19,489 |
| 28 | 413 | 118 | 29% | −$32,681 |
| 29–32 | 253 | 79 | 31% | −$10,214 |
| 33–34 | 653 | 223 | 34% | −$35,800 |
| 36 | 137 | 50 | 36% | −$9,189 |
| 38–45 | 154 | 64 | 42% | −$4,429 |
| 56 | 81 | 46 | 57% | −$1,199 |
| 63 | 130 | 83 | 64% | +$14,230 |
| 64–99 | 159 | 135 | 85% | +$21,818 |

| Block at | Blocked | Precision | Fraud-dollar recall | Legitimate dollars blocked | Net |
|---|---|---|---|---|---|
| 35 (experiment 1's 1% legit-dollar budget) | 661 | 57.2% | 19.0% | 0.67% | +$21,231 |
| **50 (chosen; same as any cut 46–56)** | **370** | **71.4%** | **15.0%** | **0.36%** | **+$34,849** |
| 57–63 (validation's net maximum) | 289 | 75.4% | 13.1% | 0.26% | +$36,048 |
| 90 (the old placeholder) | 48 | 100% | 0.9% | 0% | +$4,780 |

**Chosen: 50**, the break-even the cost model predicts. On validation it is $1,199 short of the maximum. That gap is one step of 81 payments, 57% fraud by count but with small-ticket fraud. Tuning the threshold to that would be tuning to noise. Experiment 1's 1% legit-dollar budget would allow 35, but every step from 35 to 45 loses money under experiment 8's accounting.

## Step 2: the review band

The review threshold is 25. That is the lowest score step with at least 25% fraud (25.4%). Every step above it has a higher fraud rate. With the product C rule the queue is 1,893 payments a month, 2.3% of payments or about 63 a day. Below 25 the steps fall to 17–22% fraud. The bar for any review rule added on top is the same 25%: it must be at least as precise as the band's weakest step.

## Step 3: rules on top of the model

Each candidate was backtested against the current set (block ≥ 50, review ≥ 25) and scored by what it changes. Candidates are the starter set's placeholders, the rules-baseline set (`rules/baseline.rules`) and segments that the validation month suggested.

| Candidate | Matched | Precision | Changed | Changed precision | Changed fraud $ | Changed legit $ | Verdict |
|---|---|---|---|---|---|---|---|
| allow: trusted domain, uid seen 30+ d, risk_score < 20 (starter) | 11,978 | 1.3% | 0 | – | – | – | changes nothing: drop |
| allow: uid seen 90+ d ago and idle 7+ d (baseline) | 3,724 | 0.8% | 7 released | 57% fraud | $499 released | $328 | releases fraud: drop |
| block: distinct cards per device 24 h > 5 (starter) | 7,658 | 7.2% | 7,496 | 5.8% | $39,494 | $649,190 | drop |
| block: card 1 h count ≥ 8 and amount > 3× 7-d mean (starter) | 77 | 6.5% | 77 | 6.5% | $2,618 | $40,015 | drop |
| block: disposable domain and amount > $500 (starter) | 250 | 1.6% | 250 | 1.6% | $3,450 | $274,278 | drop |
| review: anonymous.com and amount > $300 (starter) | 534 | 1.3% | 524 | 1.3% | $4,623 | $376,308 | drop (below base rate) |
| review: uid amount ratio 7 d > 5 and amount > $200 (starter) | 563 | 1.8% | 559 | 1.4% | $3,807 | $374,127 | drop |
| review: C, no device_info, risk_score ≥ 30 (starter) | 144 | 53.5% | 0 | – | – | – | covered by review ≥ 25: drop |
| review: C and amount > $50 (baseline) | 2,171 | 21.2% | 1,750 | 12.5% | $20,025 | $136,488 | drop |
| review: billing_country_code != 87 (baseline) | 413 | 13.8% | 357 | 8.1% | $1,765 | $20,939 | drop |
| review: W, credit, uid 24 h spend > $500 (baseline) | 648 | 14.8% | 528 | 10.2% | $25,231 | $353,532 | drop |
| review: Discover and amount > $100 (baseline) | 543 | 17.1% | 462 | 10.0% | $18,921 | $172,610 | drop |
| review: C, credit, recipient gmail.com, amount > $30 (baseline block) | 788 | 35.0% | 457 | 21.7% | $5,836 | $20,678 | drop |
| review: distinct cards per email 24 h > 20, not trusted (starter shadow) | 30,784 | 2.6% | 30,196 | 2.0% | $97,291 | $4,261,514 | drop |
| review: risk_score ≥ 15 and product C | 1,330 | 39.8% | 381 | 22.6% | $5,097 | $12,855 | below bar |
| review: risk_score ≥ 10 and product C | 2,586 | 28.1% | 1,637 | 17.2% | $15,650 | $60,740 | below bar |
| **review: risk_score ≥ 15, C, amount > $50** | **531** | **53.7%** | **110** | **39.1%** | **$3,820** | **$6,746** | **keep** |
| review: risk_score ≥ 10, C, amount > $50 | 886 | 40.9% | 465 | 25.8% | $11,146 | $34,449 | at the bar; the ≥ 15 version is stronger |
| review: risk_score ≥ 15, C, no device_info | 464 | 42.7% | 175 | 30.3% | $3,095 | $5,003 | 22.1% once the kept rule is in: drop |
| review: risk_score ≥ 15, C, credit, recipient gmail.com | 641 | 45.4% | 143 | 27.3% | $2,025 | $4,509 | 24.3% once the kept rule is in: drop |
| review: risk_score ≥ 15, C, card seen again within 180 s | 234 | 44.0% | 52 | 26.9% | $732 | $1,200 | 17.5% once the kept rule is in: drop |
| review: risk_score ≥ 15, Discover | 152 | 44.7% | 47 | 27.7% | $4,854 | $10,711 | 13 frauds: shadow |
| review: risk_score ≥ 15, Discover, amount > $100 | 111 | 52.3% | 30 | 36.7% | $4,774 | $9,616 | 11 frauds, extra tuned knob: not used |
| review: risk_score ≥ 15, credit | 1,923 | 34.4% | 523 | 21.4% | $20,075 | $73,056 | below bar |
| review: risk_score ≥ 15, amount > $300 | 496 | 33.5% | 147 | 17.7% | $15,632 | $75,826 | below bar |
| review: risk_score ≥ 15, product W / H / R / S | | | 365 / 53 / 72 / 36 | 19.5% / 9.4% / 11.1% / 19.4% | | | below bar |
| review: risk_score ≥ 15, uid missing | 1,787 | 36.7% | 495 | 20.6% | $8,881 | $37,576 | below bar |
| review: risk_score ≥ 15, mobile | 853 | 38.1% | 184 | 22.3% | $3,078 | $9,290 | below bar |
| block: risk_score ≥ 25, C, amount > $50 (review → block) | 421 | 57.5% | 322 | 49.4% | $14,954 | $16,743 | net +$596, break-even: shadow |
| block: risk_score ≥ 35 and C (review → block) | 285 | 64.9% | 126 | 47.6% | $3,719 | $4,055 | net +$564: drop |
| block: C, credit, recipient gmail.com, > $30 (baseline) | 788 | 35.0% | 737 | 32.3% | $15,896 | $32,398 | net −$12,932: drop |

The pattern is that the raw-field rules which work alone (baseline.rules) add little once the model is in, because the model has already found that fraud. What does add is product C at the edge of the review band. Nearly every product C payment lacks `billing_region`, so its customer (`uid`) velocity features are missing, and the model is a little under-confident there. Nothing passed the bar for block.

## The rule set

Incremental numbers are leave-one-out: each rule against all the other live rules in the final file. Shadow rules are measured against the live set.

| Rule | Validation matched | Precision | Changed (incremental) | Incremental precision | Incremental fraud $ | Incremental legit $ |
|---|---|---|---|---|---|---|
| `block if :risk_score: >= 50` | 370 | 71.4% | 370 (review → block) | 71.4% | $69,189 | $38,299 |
| `review if :risk_score: >= 25` | 2,153 | 39.1% | 1,461 (allow → review) | 28.7% | $77,306 | $195,990 |
| `review if :product_code: = "C" and :amount: > 50 and :risk_score: >= 15` | 531 | 53.7% | 110 (allow → review) | 39.1% | $3,820 | $6,746 |
| shadow: `block if :product_code: = "C" and :amount: > 50 and :risk_score: >= 25` | 421 | 57.5% | 322 (review → block) | 49.4% | $14,954 | $16,743 |
| shadow: `review if :card_network: = "discover" and :risk_score: >= 15` | 152 | 44.7% | 47 (allow → review) | 27.7% | $4,854 | $10,711 |

For the review rules, "incremental legit $" is legitimate dollars sent to review (payout held), not blocked.

No allow rule. The starter allow rule changes no decision, because nothing in the set fires below `risk_score` 15. The rules-baseline allow rule would release 7 flagged payments, 4 of them fraud.

## Set totals on validation (`validation_default.json`, `validation_model_only.json`)

| Set | Flagged | Blocked | Reviewed | Precision (flagged) | Fraud recall | FPR | Fraud-dollar recall | Legit $ blocked | Legit $ reviewed |
|---|---|---|---|---|---|---|---|---|---|
| Old `default.rules` (placeholders) | 8,978 | 7,957 | 1,021 | 8.8% | 27.7% | 10.14% | 26.6% | 12.96% blocked or reviewed | |
| **New `default.rules`** | **2,263** | **370** | **1,893** | **39.1%** | **31.1%** | **1.71%** | **35.7%** | **0.36%** | **2.05%** |
| **`model_only.rules`** | **370** | **370** | **0** | **71.4%** | **9.3%** | **0.13%** | **15.0%** | **0.36%** | **0** |
| Model thresholds only (block ≥ 50, review ≥ 25) | 2,153 | 370 | 1,783 | 39.1% | 29.5% | 1.62% | 34.9% | 0.36% | 1.99% |

The old set also allowed 11,978 payments outright. Its `distinct_cards_per_device_24h > 5` block rule alone blocked 7,613 payments at 6.8% fraud, because `device_info` is a model string ("Windows"), not a device.

The product C rule adds 43 frauds (1.5 points of fraud recall) and $3,820 of fraud dollars on top of the model's thresholds, for 110 more reviews. That is a small gain, and it is the honest size of what hand-written rules add over this model on this month.

## Files and commands

- `sweep_valid.json`: `backtest sweep -table data/cache/table_scored_real.rgt -from-day 121 -to-day 151 -maturity-days 0 -json`.
- `validation_default.json`, `validation_model_only.json`: `backtest rulestats -table data/cache/table_scored_real.rgt -lists rules/lists.json -rules <file> -split valid -json`.
- Incremental effects: `backtest run -table data/cache/table_scored_real.rgt -from-day 121 -to-day 151 -maturity-days 0 -lists rules/lists.json -current <the other rules> -rule '<rule>'`.
- Both files pass `go run ./cmd/rulecheck -lists rules/lists.json <file>`.
- Provenance: commit f4c2c3d plus this session's uncommitted changes, 2026-09-24 UTC, Apple M3 Pro, go1.26.5. No timings. SHA-256 at writing: `rules/default.rules` 3f5283ec773e74d70ba17552a0b466ef3f23bd93ab4e8d441c567ed24c8eeb85, `rules/model_only.rules` 6957c4f2ef8a050c8ff2767905e2c9d6307a160c6b27298c4ba191b0783546b5.
