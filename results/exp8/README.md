# Experiment 8: end to end through Clearinghouse

The IEEE-CIS test month (92,427 payments, simulated 2018-05-01 to 2018-06-09) was replayed through Clearinghouse's API with Clearinghouse's `bin/exp-replay`, run by the Clearinghouse session on 2026-09-25. On every confirm, Clearinghouse called RiskGate's `/v1/assess`. Fraud-labelled payments used Clearinghouse's disputing test card, so disputes arrived in simulated time and flowed back to RiskGate as signed `charge.dispute.created` webhooks. Each policy got a fresh database and its own RiskGate process, started from its own copy of the warm snapshot (`scripts/experiments/exp8_warm.sh`: velocity state as of the start of the test month). The model hash `a75d7ac4…ce540` was checked through `/v1/info` before each run. RiskGate binary from commit `016337a`.

Source files, owned by Clearinghouse: `results/exp8-replay/20260925T020100Z-full-{none,model_only,model_rules}/` in the Clearinghouse repo. The numbers below were copied from them after reading them directly. Aggregates only.

| | No risk checks | Model threshold (`rules/model_only.rules`) | Model plus rules (`rules/default.rules`) |
|---|---|---|---|
| Payments | 92,427 | 92,427 | 92,427 |
| Blocked | 0 | 384 ($100,221.48) | 384 ($100,221.48) |
| Fraud caught (blocked, labelled fraud) | 0 | 227 ($59,862.04) | 227 ($59,862.04) |
| Legitimate revenue blocked | 0 | 157 ($40,359.44) | 157 ($40,359.44) |
| Disputes | 3,213 ($487,462.76) | 2,986 ($427,600.72) | 2,986 ($427,600.72) |
| Dispute losses, amount plus an **assumed** $15 fee | $535,657.76 | $472,390.72 | $472,390.72 |
| Reviews (payout held until review) | 0 | 0 | 2,496 ($413,409.07 of merchant net held) |
| `risk_unavailable` (timed out, failed open) | n/a | 24 | 8 |
| **Net of checks** | $0 | **+$22,907.60** | **+$22,907.60** |

- **Dispute losses fell 11.8%** ($63,267.04), at the cost of $40,359.44 of legitimate revenue blocked. The block rule's precision was 59% on the test month (227 of 384), against 71% on validation, where its threshold was chosen.
- **The two model policies have identical money outcomes by construction.** Both block on `risk_score >= 50`. `default.rules` adds only review rules, and in Clearinghouse a review lets the payment through and holds the merchant's payout of those funds. So the difference between them is the 2,496 holds, not dollars lost. Reviews resolve nothing in this simulation: nobody works the queue.
- **The label loop works end to end.** Clearinghouse delivered all 2,986 dispute events and 92,043 `payment_intent.succeeded` events to RiskGate's webhook receiver, and every delivery succeeded. Clearinghouse's dispute count equals the simulator's, with 0 disputes on legitimate payments, and 0 ledger invariant violations in all three runs.
- **What is real and what is simulated.** The transactions, amounts and fraud labels are real IEEE-CIS data. Everything around them is a simulation driven by that data: the ledger, the disputes, their timing, and the $15 fee (an assumption, Stripe's published US dispute fee).
- **Latency is not quotable.** The three runs were concurrent on a shared machine, with 1-minute load around 30 at the start, and the Mac slept for about 75 minutes mid-run. For the record only: the risk step was p50 0.68 to 0.75 ms and p99 4.3 to 5.2 ms, 0.5 to 0.7% of confirm time.
