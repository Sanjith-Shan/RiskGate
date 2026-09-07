# Experiment 7: label delay (SIMULATED)

**Every number here is SIMULATED.** IEEE-CIS fraud labels are final and carry no arrival time. For this experiment each fraud label gets a simulated dispute arrival time, drawn from an assumed delay distribution. The distribution is `backtest.DefaultDelay`: lognormal, median 30 days, sigma 0.5, capped at 120 days, and a draw past the cap never arrives. That is an assumption, not a measurement.

- **Command.** `go run ./cmd/backtest labeldelay -table data/export_real/features.table -as-of-day 151 -rule '<rule>' -json`, plus `-median-days` and `-maturity-days` for the sensitivity rows. Raw output is in `label_delay.json`.
- **Data.** IEEE-CIS (real), `data/export_real/features.table`, 590,540 rows, exact velocity state. The rules use no `risk_score`.
- **Setup.** "Today" (as-of) is the end of the validation month, day 151 after the TransactionDT epoch (May 1, 2018 under the assumed 2017-12-01 epoch). The naive backtest covers the last 30 days before as-of, which is the validation month, with labels as of today. The matured backtest covers 30 days ending 60 days before as-of (Jan 31 to Mar 2, 2018, in the training months), with the same labels. Each is compared with its own window's final labels. **No test-month row is used.**
- **Rules.** Four rules from `rules/baseline.rules`, each backtested alone, with no rules in force.
- **Provenance.** See `provenance.json`: commit, date, machine (Apple M3 Pro, 6P+6E cores), Go 1.26.5. This experiment measures no time, so machine load does not matter.

## Result (SIMULATED)

Each cell reads "as reported / true". Understatement is 1 minus reported over true fraud dollars caught.

| Rule | Naive: fraud caught | Naive: fraud $ | Naive understatement | Matured: fraud caught | Matured: fraud $ | Matured understatement | Naive precision, reported vs true |
|---|---|---|---|---|---|---|---|
| `block if :product_code: = "C" and :card_type: = "credit" and :recipient_email_domain: = "gmail.com" and :amount: > 30` | 54 / 276 | $3,970 / $19,495 | 79.6% | 246 / 252 | $16,565 / $17,239 | 3.9% | 6.9% vs 35.0% |
| `review if :product_code: = "C" and :amount: > 50` | 86 / 461 | $7,736 / $42,447 | 81.8% | 316 / 325 | $27,133 / $28,164 | 3.7% | 4.0% vs 21.2% |
| `review if :product_code: = "W" and :card_type: = "credit" and :uid_amount_sum_24h: > 500` | 12 / 96 | $6,834 / $56,975 | 88.0% | 144 / 154 | $52,246 / $55,845 | 6.4% | 1.9% vs 14.8% |
| `review if :card_network: = "discover" and :amount: > 100` | 12 / 93 | $5,855 / $49,320 | 88.1% | 77 / 81 | $37,852 / $39,821 | 4.9% | 2.2% vs 17.1% |

Under this assumption, a naive last-30-days backtest credits these rules with 12% to 20% of the fraud dollars they actually catch. Their precision looks 5 to 8 times lower than it is. With the default 60-day maturity window, the understatement falls to 4% to 6%.

## Sensitivity to the assumption (SIMULATED)

Rule `review if :product_code: = "C" and :amount: > 50`, sigma 0.5 throughout. The cells are fraud-dollar understatement.

| Assumed median delay | Naive, last 30 days | Matured, 30-day maturity | Matured, 60-day maturity | Matured, 90-day maturity |
|---|---|---|---|---|
| 15 days | 47.0% | 3.6% | 0.0% | 0.0% |
| 30 days (default) | 81.8% | 28.0% | 3.7% | 0.6% |
| 45 days | 94.8% | 60.2% | 17.4% | 5.2% |

How big the naive error is depends almost entirely on the assumed delay. The direction does not: a recent-window backtest always understates a rule's catch. A 60-day window is enough only if disputes mostly arrive within about a month. If the real median is 45 days, 60 days still understates by about a sixth.
