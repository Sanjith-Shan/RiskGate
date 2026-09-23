# Experiment 6: backtest speed on the real table

> **Re-run before quoting.** These runs were on a shared machine under heavy load. The 1-minute load average was 13 to 46 during the runs, and every run's `uptime` is in `backtest_speed.json` under `loads`. Timings of identical code moved by up to 20% between runs (the row-at-a-time rows are the same code before and after). Treat every number as indicative until it is re-run on a quiet machine.

- **Command.** `go run ./cmd/backtest bench -table <table> -reps 9 -json`, and a second set with `-proposed 'block if :product_code: = "C" and :amount: > 50'`. "Before" is the same command built from the tree before this session's evaluator changes.
- **Machine.** Apple M3 Pro, 6 performance and 6 efficiency cores, 12 CPUs, GOMAXPROCS=12, go1.26.5 darwin/arm64. Commit and date are in `provenance.json`. The tree had this session's uncommitted changes.
- **Data.** IEEE-CIS (real), 590,540 rows. Two tables: `export` is `data/export_real/features.table`, as exported, with `risk_score` NaN on every row. `scored` is `data/cache/table_scored_real.rgt`, the same replay with `risk_score` from `models/ieee` (`cmd/riskgate table -model models/ieee`).
- **Method.** Before and after were interleaved, 3 runs each per table. Each run times each case 9 times after one warm-up. "Best" is the best single timing over the 3 runs. "Median" is the median of the 3 runs' medians. Row-at-a-time is `RuleSet.Evaluate` over a row store built in advance and not timed. Vectorized is `EvaluateRuleSet`, which also produces every rule's full match bitmap (the backtester needs them for overlaps). Row-at-a-time does not.
- **Correctness.** In every "after" run, `backtest.CheckRuleSet` found vectorized and row-at-a-time decisions identical (action and deciding rule) on all 590,540 rows.

## Results, `export` table (best / median, ms)

| Workload | Evaluator | Threads | Before | After |
|---|---|---|---|---|
| One rule | Vectorized | 1 | 1.58 / 1.68 | 1.72 / 1.87 |
| One rule | Vectorized | 12 | 1.04 / 1.19 | 0.36 / 0.53 |
| One rule | Row-at-a-time | 1 | 34.6 / 47.4 | 28.9 / 36.6 |
| One rule | Row-at-a-time | 12 | 7.94 / 9.25 | 8.44 / 10.8 |
| 50-rule set | Vectorized | 1 | 47.6 / 48.6 | **31.4 / 32.3** |
| 50-rule set | Vectorized | 12 | 7.94 / 9.03 | **3.83 / 4.98** |
| 50-rule set | Row-at-a-time | 1 | 78.3 / 80.3 | 78.2 / 79.3 |
| 50-rule set | Row-at-a-time | 12 | 10.8 / 13.4 | 10.5 / 13.7 |
| Page: one proposed rule vs cached 50-rule baseline, same period as last run | `Backtester.Run` | 12 | 3.57 / 3.88 | **0.60 / 0.87** |
| Page: the same, first run over a new period | `Backtester.Run` | 12 | (same as above) | 3.59 / 3.79 |

## Results, `scored` table (best / median, ms)

| Workload | Evaluator | Threads | Before | After |
|---|---|---|---|---|
| One rule | Vectorized | 1 | 1.57 / 1.81 | 1.83 / 3.02 |
| One rule | Vectorized | 12 | 1.07 / 1.69 | 0.47 / 1.08 |
| 50-rule set | Vectorized | 1 | 48.2 / 56.1 | **32.6 / 41.6** |
| 50-rule set | Vectorized | 12 | 8.13 / 14.2 | **4.83 / 5.98** |
| 50-rule set | Row-at-a-time | 1 | 56.8 / 78.8 | 57.6 / 70.3 |
| 50-rule set | Row-at-a-time | 12 | 9.18 / 10.4 | 9.58 / 12.4 |
| Page, same period (proposed rule on `risk_score`) | `Backtester.Run` | 12 | 3.75 / 4.49 | 0.58 / 0.70 |
| Page, same period, `block if :product_code: = "C" and :amount: > 50` | `Backtester.Run` | 12 | n/a | 0.92 / 1.06 |

## What changed and why

**Before.** On the real table the 50-rule set ran vectorized in 47.6 ms on one thread against 78 ms row-at-a-time, 1.6x, and 7.9 ms against 10.8 ms on 12 threads. A per-rule timing on the export table found the cause. The three rules with a numeric `in` list (`:billing_region: in @blocked_regions`, `:billing_country_code: in @watched_countries`) took 5.7 to 6.6 ms each. Every other rule took 0.34 to 3.3 ms. The kernel looped over rows with a short-circuiting `||` for lists of up to 8 values and a per-row binary search for longer ones. Both branch on the data, and these columns hold many distinct values, so the branches mispredicted.

Three changes, all in `internal/backtest`:

1. **Branch-free numeric `in`** (`ops.go`). A list of up to 4 values is tested one list value at a time against a whole 64-row word. A longer list is compiled into a collision-free hash table (`numSet`): a multiplicative hash of the value's bits, with -0 folded to +0 and NaN in empty slots. A lookup is one multiply, one load and one compare. Those three rules went to 0.76 to 1.09 ms. Summed per-rule time for the 50 rules on one thread went from 45.5 ms to 33 ms.
2. **Decisions made per chunk, in parallel** (`ruleset.go`). `EvaluateRuleSet` used to evaluate all masks and then decide Radar's order in one sequential pass over whole bitmaps, about 4 to 5 ms. That pass was most of the 12-thread time. Each worker now decides its chunk right after evaluating it. This is why 12 threads improved more than one thread did.
3. **The page's window is reused** (`report.go`). `Backtester.Run` resolved the backtest period on every call: a pass over all 590,540 rows to build the counted, fraud and legitimate bitmaps and sum the period's dollars. That took about two thirds of a Run. The Backtester now keeps the last resolved window and reuses it while the options that define it are unchanged, as they are while an analyst edits a rule. A new period still pays the pass (3.6 ms). A test checks that reports are byte-identical to a fresh Backtester's, including under concurrent runs.

**Tried and not kept.** Skipping rows already decided (evaluating each block rule only on rows no allow or earlier block rule claimed, and each review rule only on rows still open) was implemented as a decisions-only path and measured on the real table. It was 3% faster on one thread (30.8 ms against 31.9 ms). After the block tier about 22% of rows are still open. That is far above the density (1 in 16) where refinement beats the full column kernels, and the claimed rows are spread across every chunk, so few chunks can be skipped entirely. It is not worth a second evaluation path, so it was removed. The existing `and` refinement (selection vector on sparse left sides) already covers the case where the left side is sparse.

**Not faster.** One rule on one thread is about the same or slightly slower after (1.58 to 1.72 ms on export, 1.57 to 1.83 ms on scored), inside the run-to-run noise seen here. That workload has no numeric `in` and a single rule, so neither change 1 nor change 2 has anything to speed up.

**Row-at-a-time depends on the data, vectorized mostly does not.** Row-at-a-time is slower on the export table (78 ms) than on the scored one (57 ms) for the same code. With `risk_score` present, the five allow rules (all conditioned on a low score) match many payments, and an allowed payment skips the other 45 rules. On the export table no allow rule can match, so every payment runs through the block rules until one matches. Vectorized evaluation runs every rule over every chunk either way, because the backtester needs every rule's full mask, so it costs about the same on both tables. The scored table is the realistic one for a live rule set.

**Headline, after, on the realistic scored table.** The 50-rule set evaluates in 32.6 ms on one thread (row-at-a-time 57.6 ms, 1.8x) and 4.8 ms on 12 threads (row-at-a-time 9.6 ms, 2.0x). The page's backtest of one proposed rule against the cached baseline takes 0.6 to 1.1 ms. Re-run on a quiet machine before quoting any of these.
