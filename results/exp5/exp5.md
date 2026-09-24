# Experiment 5: latency under load

Command `scripts/experiments/exp5.sh` (REPS=2, DURATION=20s, WARMUP=5s, deadline 50ms), git commit `0c341c108c02d2d0406d3a56c39fe057d7d3c17b`, 2026-09-24T23:17:08Z to 2026-09-24T23:29:55Z.

Machine: Apple M3 Pro (6P+6E cores, 12 logical CPUs), go1.26.5. GOMAXPROCS: server default (all logical CPUs), loadgen default (all logical CPUs). Server and load generator shared the machine.

**Load.** 1-minute load average before each rate: min 10.04, median 83.69, max 144.59 (on 12 logical CPUs). The machine was shared with other heavy jobs. **These numbers are not quotable; re-run `scripts/experiments/exp5.sh` on a quiet machine first.**

Data: test month of real IEEE-CIS transactions (data/export_real/test_replay.jsonl, 92,427 lines, is_fraud stripped), sent round-robin with payment_id rewritten unique per send. Model `models/ieee` (954 trees), rules/default.rules + rules/lists.json. exact velocity state, fresh at the start of each configuration; it keeps filling across that configuration's rates. After the 92,427-line file wraps, event times repeat and the state records them at each key's latest time (late-event rule).

## Highest rate with p99 inside the deadline

| Config | Per repetition (req/s; none = no rate passed) | Median (none counted as 0) |
|---|---|---|
| locked | none, 1000 | 500 |
| syncmap | none, 3000 | 1500 |
| sharded-4 | none, none | 0 |
| sharded-64 | none, none | 0 |

**Generator health.** In 22 of 22 rate steps the load generator's own p99 send lag was over 1 ms, meaning it could not send on schedule. Where that happens the latency includes CPU starvation of the generator and the server by the rest of the machine, not only RiskGate's cost. The configurations ran one after another while the load changed, so load and configuration are confounded and the ranking between configurations is not meaningful from this run.

## Per rate (medians over repetitions; latency in ms from intended send time)

### locked

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 2.911 | 2261.503 | 492.543-4030.463 | 2476.031 | 53.023 | 0/2 |
| 1000 | 1000 | 1.752 | 675.519 | 36.223-1314.815 | 752.991 | 10.679 | 1/2 |
| 2000 | 2000 | 1.375 | 874.495 | 874.495-874.495 | 1120.255 | 451.839 | 0/1 |
| 3000 | 3000 | 1.153 | 655.359 | 655.359-655.359 | 961.535 | 192.255 | 0/1 |

### syncmap

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 2.144 | 526.511 | 23.903-1029.119 | 616.799 | 9.919 | 1/2 |
| 1000 | 1000 | 1.525 | 404.591 | 42.207-766.975 | 488.575 | 68.607 | 1/2 |
| 2000 | 2000 | 5.747 | 3454.975 | 3454.975-3454.975 | 4030.463 | 460.287 | 0/1 |
| 3000 | 3000 | 0.843 | 33.631 | 33.631-33.631 | 80.191 | 7.487 | 1/1 |
| 4000 | 4000 | 0.956 | 83.391 | 83.391-83.391 | 176.127 | 17.279 | 0/1 |
| 5000 | 5000 | 1.309 | 141.439 | 141.439-141.439 | 269.823 | 24.447 | 0/1 |

### sharded-4

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.496 | 75.391 | 71.295-79.487 | 159.103 | 17.887 | 0/2 |
| 1000 | 1000 | 1.792 | 220.287 | 79.615-360.959 | 286.975 | 25.379 | 0/2 |

### sharded-64

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 2.033 | 626.655 | 92.095-1161.215 | 748.031 | 8.695 | 0/2 |
| 1000 | 1000 | 1.465 | 77.231 | 50.783-103.679 | 126.111 | 18.491 | 0/2 |

## Single-request cost in process (real model, real request stream)

`go test -bench` in internal/service with RISKGATE_BENCH_MODEL and RISKGATE_BENCH_REQUESTS; see results/exp5/bench.txt.

| Benchmark | Runs | ns/op median | min | max |
|---|---|---|---|---|
| BenchmarkAssessPipeline | 5 | 426764 | 373983 | 919202 |
| BenchmarkAssessHandler | 5 | 451446 | 363693 | 1143404 |
| BenchmarkAssessHTTP | 5 | 985118 | 803672 | 1166144 |
| BenchmarkModel/Score | 5 | 305235 | 236811 | 384416 |
| BenchmarkModel/Contributions | 5 | 370548 | 323014 | 417492 |
| BenchmarkModel/ScoreContributions | 5 | 369021 | 319345 | 388802 |

