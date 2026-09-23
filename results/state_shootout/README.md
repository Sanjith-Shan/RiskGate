# Experiment 4: velocity state shootout on the real data

> **Timings: re-run before quoting.** The machine was shared and heavily loaded. The 1-minute load average was 12 at the start of the run and 40 at the end (`state.json` → `load`). Timings of the same state moved by 30% or more between repetitions. Memory, feature error and the metrics are not timings, and they are deterministic.

- **Command.** `go run ./cmd/experiments/state -model models/ieee -reps 7 -out results/state_shootout/state.json`. The command was written for this experiment. Raw output is in `state.json`, aggregates only.
- **Data.** IEEE-CIS (real), 590,540 payments, loaded through `data.Load` and replayed in (TransactionDT, TransactionID) order: 1,729,036 keyed (payment, entity) operations.
- **Machine.** Apple M3 Pro (6P+6E), go1.26.5, one goroutine. Commit and date are in `provenance.json`. The tree had this session's uncommitted changes.
- **States.** Default configurations: `NewExact(0)`, `NewBucketed(BucketedConfig{})`, `NewSketch(SketchConfig{})` (count-min 4 x 16,384, HyperLogLog precision 6, 2 x 1,024), idle TTL 30 days.

## Memory, at the end of a full replay (and at the peak, sampled every 65,536 payments)

| State | Live keys | Estimated bytes (`Stats`) | Bytes per live key | Peak estimated bytes | Heap measured after GC |
|---|---|---|---|---|---|
| Exact | 48,245 | 18.4 MB | 380 | 28.5 MB (74,808 keys) | 24.5 MB |
| Bucketed | 48,245 | 29.6 MB | 613 | 45.1 MB | 36.3 MB |
| Sketch | (cannot count) | 64.2 MB, fixed | 1,331 if divided by exact's live keys | 64.2 MB | 64.2 MB |

At IEEE-CIS scale the exact state is the smallest. Most keys have a few payments a week, and a deque of a few events is smaller than the bucketed state's per-window rings or a sketch sized for much more traffic. The bucketed state's advantage is a bound per key under a burst, and this data has no burst big enough to show it.

## Idle-key eviction

| State | Live keys with eviction / without | Estimated bytes with / without | Heap with / without | Features identical with and without |
|---|---|---|---|---|
| Exact | 48,245 / 214,468 | 18.4 / 74.1 MB | 24.5 / 77.7 MB | yes, every value bit-identical |
| Bucketed | 48,245 / 214,468 | 29.6 / 126.4 MB | 36.3 / 132.6 MB | yes |
| Sketch | n/a | 64.2 / 64.2 MB | 64.2 / 64.2 MB | yes |

Eviction cuts the keyed states' memory by about 4x over six months and changes no feature. That is checked with a hash of every numeric feature of every row across the two replays.

## Speed, ns per operation, one goroutine (median / min of 7)

| State | Add (with eviction sweeps) | ReadAdd (the engine's operation) | Read, as ReadAdd minus Add in context |
|---|---|---|---|
| Exact | 349 / 326 | 347 / 315 | about 0 (median -6, runs -36 to 238) |
| Bucketed | 320 / 308 | 339 / 318 | about 0 (median -21, runs -281 to 906) |
| Sketch | 462 / 355 | 1,353 / 1,067 | 937 / 588 |

A read in context comes right after the key's previous update, because scoring always reads and adds together. For the exact and bucketed states it costs almost nothing beyond the add: the difference is inside the noise. The sketch's read costs about 1 µs, because it sums count-min cells over every bucket of every window.

An earlier run of the same command measured reads differently. It read the keys of the last 20,000 payments against a state frozen just before them, so each read could come a day or more after the key's last update. Those reads cost 2,954 ns (exact), 242 ns (bucketed) and 2,458 ns (sketch). The exact state is slow there because a read cannot drop expired events, so it walks all of them. A read-only `Score` long after a key's last update would pay that.

## Feature error against exact (every row, `features.CompareStates`)

Pairs are (row, feature) with both values present. "Exact" means bit-identical. Relative error is |approx - exact| / max(|exact|, 1). "Missing mismatch" is one side missing and the other present.

| State | Family (features) | Exact share | Mean relative error | Max absolute error | Over / under exact | Missing mismatch |
|---|---|---|---|---|---|---|
| Bucketed | count (12) | 76.2% | 0.44% | 329 payments | 23.8% / 0 | 0 |
| Bucketed | sum (12) | 76.2% | 7.4% | $77,000 | 23.8% / 0 | 0 |
| Bucketed | distinct cards (2) | 44.8% | 0.55% | 24 cards | 55.2% / 0 | 0 |
| Bucketed | mean, ratio (8) | 55.0% | 0.16% | 1,036 | 22.5% / 22.5% | 1,862 |
| Bucketed | recency (8) | 100% | 0 | 0 | 0 / 0 | 0 |
| Sketch | count (12) | 61.8% | 4.9% | 1,348 payments | 38.2% / 0 | 0 |
| Sketch | sum (12) | 61.8% | 168% | $232,400 | 38.2% / 0 | 0 |
| Sketch | distinct cards (2) | 2.5% | 9.4% | 281 cards | 53.7% / 43.8% | 0 |
| Sketch | mean, ratio (8) | 32.4% | 2.1% | 3,558 | 33.8% / 33.8% | 162,484 |
| Sketch | recency (8) | 88.3% | 62,360% | 181 days | 9.3% / 2.4% | 392,560 |

As designed, the bucketed state never undercounts: its counts and sums are over only, by at most one trailing bucket. The sketch's worst feature is recency. The min/max sketches that track first and last seen collide, so an unseen customer often reads as seen, with a first-seen time from another key. `uid_seconds_since_first` is bit-exact on only 23.5% of rows. Its HyperLogLog distinct counts are off in both directions. Per-feature numbers are in `state.json` → `error_by_feature`.

## What the approximation does to the model (validation month)

The model is `models/ieee`, trained by `python/train.py` on exact-state features and **not retrained**. Each state computes features through the same replay, stopping at the end of the validation month, so no test-month payment is replayed or scored. The score is the raw LightGBM log-odds from the Go evaluator. There are 83,571 validation payments, 2,850 of them fraud. This is what swapping the state under a deployed model would do. Retraining on each state's features might recover some of the difference.

| State | ROC-AUC | PR-AUC (average precision) | Fraud recall at 1% FPR | Validation scores that differ from exact |
|---|---|---|---|---|
| Exact | 0.84505 | 0.29827 | 22.49% | 0 |
| Bucketed | 0.84500 | 0.29820 | 22.49% | 34.0% |
| Sketch | 0.83135 | 0.28667 | 22.77% | 96.6% |

The exact row reproduces `python/train.py`'s own validation numbers for the chosen model (ROC-AUC 0.8450482787, PR-AUC 0.2982651047, recall 0.2249122807) to about 1e-13. That checks the Go replay, the Go evaluator and these metric definitions (scikit-learn's) against the Python pipeline.

**Finding.** The bucketed ring costs nothing measurable here. It changes a third of the scores slightly, and the metrics move in the fifth decimal. The sketch costs 0.014 ROC-AUC and 0.012 PR-AUC. Its recall at 1% FPR is 0.28 points *higher* (8 more of 2,850 frauds). One operating point moving that little is noise, and the threshold-free metrics are the clearer signal. The validation month was also the model's early-stopping set, so its absolute level is optimistic for all three states alike. The comparison between states is not affected.
