# Experiment 3: two evaluators, one answer, on the real table

- **Command.** `go run ./cmd/backtest difftest -table <table> -rules 10000`. It generates 5,000 rules with rule-seed 1 and 5,000 with seed 2 whose constants are drawn from the table (`backtest.Ground`), at depths 0 to 6. Each rule is evaluated on every row by the closure evaluator (`rules.Compile`, the online path) and the vectorized evaluator (`backtest.CompileVector`), and the two bitmaps are compared.
- **Data.** IEEE-CIS (real), all 590,540 rows. This experiment measures correctness only, so every split is used. `export` is `data/export_real/features.table` (`risk_score` NaN on every row). `scored` is `data/cache/table_scored_real.rgt` (the same replay, with `risk_score` from `models/ieee`).
- **Code.** The evaluator as changed in this session (branch-free numeric `in`, per-chunk decisions; see `results/backtest_speed/README.md`). The commit is in `provenance.json`. The tree had this session's uncommitted changes.
- **Machine and load.** Apple M3 Pro (6P+6E), go1.26.5, GOMAXPROCS=12. The 1-minute load average was 15 to 46 during the runs. Wall times are recorded but are not a result.

## Rule level

| Table | Rules generated | Grounded in table | Non-trivial rules | Rows per rule | Rule-row pairs checked | Pairs where both matched | Disagreements | Wall time |
|---|---|---|---|---|---|---|---|---|
| export | 10,000 | 1,985 | 5,576 | 590,540 | 5,905,400,000 | 1,461,187,769 | **0** | 42.5 s |
| scored | 10,000 | 1,971 | 5,676 | 590,540 | 5,905,400,000 | 1,468,463,892 | **0** | 46.7 s |

A trivial rule matches no row or every row: 4,424 on export and 4,324 on scored. Those still exercise missing-value handling but split nothing, so the non-trivial count is the informative one. Grounding succeeded for about 40% of the rules asked for. The rest either had no literal next to an attribute or no longer checked after substitution.

An earlier 6,000-rule run on the export table, with the evaluator before this session's changes, also found 0 disagreements (3,310 non-trivial rules, 3,543,240,000 pairs).

## Rule-set level

`backtest.CheckRuleSet` compares `EvaluateRuleSet` (vectorized, Radar order decided per chunk) with `RuleSet.Evaluate` on every row. It checks the action and the deciding rule. Three rule sets, both tables, all 590,540 rows each:

| Rule set | export | scored |
|---|---|---|
| `RealisticRuleSet` (50 rules) | 0 mismatches | 0 mismatches |
| `rules/baseline.rules` (8) | 0 mismatches | 0 mismatches |
| `rules/default.rules` (10; `@disposable_domains` replaced by a stand-in list) | 0 mismatches | 0 mismatches |

The 50-rule check also runs inside every `backtest bench` run (experiment 6).

## Bugs found

None on real data. The new numeric-`in` kernels got their own edge tests before these runs. Six edge rules were added to `TestDifferentialEdgeRules`, covering short and hashed lists, negation, -0, integers past 2^53 and a 30-digit literal. `TestNumSetMatchesSearch` checks the hash table against binary search on 200 lists with signed zeros, NaN, infinities and subnormals.
