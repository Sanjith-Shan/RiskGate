# Experiment 5: latency under load

Command `scripts/experiments/exp5.sh` (REPS=2, DURATION=20s, WARMUP=5s, deadline 50ms), git commit `2e59251316e7bfe67102862b9f942e05896214a4`, 2026-09-23T23:44:04Z to 2026-09-24T00:11:28Z.

Machine: Apple M3 Pro (6P+6E cores, 12 logical CPUs), go1.26.5. GOMAXPROCS: server default (all logical CPUs), loadgen default (all logical CPUs). Server and load generator shared the machine.

**Load.** 1-minute load average before each rate: min 10.49, median 38.75, max 66.19 (on 12 logical CPUs). The machine was shared with other heavy jobs. **These numbers are not quotable; re-run `scripts/experiments/exp5.sh` on a quiet machine first.**

Data: test month of real IEEE-CIS transactions (data/export_real/test_replay.jsonl, 92,427 lines, is_fraud stripped), sent round-robin with payment_id rewritten unique per send. Model `models/ieee` (954 trees), rules/default.rules + rules/lists.json. exact velocity state, fresh at the start of each configuration; it keeps filling across that configuration's rates. After the 92,427-line file wraps, event times repeat and the state records them at each key's latest time (late-event rule).

## Highest rate with p99 inside the deadline

| Config | Per repetition (req/s; none = no rate passed) | Median (none counted as 0) |
|---|---|---|
| locked | 1000, 500 | 750 |
| syncmap | none, 1000 | 500 |
| sharded-1 | 8000, 1000 | 4500 |
| sharded-4 | 5000, 1000 | 3000 |
| sharded-16 | 3000, 3000 | 3000 |
| sharded-64 | none, 1000 | 500 |

**Generator health.** In 52 of 58 rate steps the load generator's own p99 send lag was over 1 ms, meaning it could not send on schedule. Where that happens the latency includes CPU starvation of the generator and the server by the rest of the machine, not only RiskGate's cost. The configurations ran one after another while the load changed, so load and configuration are confounded and the ranking between configurations is not meaningful from this run.

## Per rate (medians over repetitions; latency in ms from intended send time)

### locked

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.639 | 14.507 | 14.415-14.599 | 94.079 | 4.062 | 2/2 |
| 1000 | 1000 | 1.747 | 49.455 | 43.071-55.839 | 88.607 | 5.917 | 1/2 |
| 2000 | 2000 | 2.174 | 128.671 | 127.103-130.239 | 198.143 | 26.199 | 0/2 |
| 3000 | 3000 | 4.383 | 178.175 | 178.175-178.175 | 319.487 | 27.823 | 0/1 |

### syncmap

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.853 | 36.759 | 5.487-68.031 | 70.807 | 4.851 | 1/2 |
| 1000 | 1000 | 1.760 | 36.793 | 6.451-67.135 | 66.695 | 6.604 | 1/2 |
| 2000 | 2000 | 1.803 | 173.311 | 173.311-173.311 | 266.751 | 49.503 | 0/1 |
| 3000 | 3000 | 3.205 | 214.143 | 214.143-214.143 | 314.111 | 24.639 | 0/1 |

### sharded-1

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.196 | 18.985 | 2.387-35.583 | 47.709 | 2.704 | 2/2 |
| 1000 | 1000 | 0.997 | 23.655 | 4.751-42.559 | 89.711 | 2.096 | 2/2 |
| 2000 | 2000 | 1.093 | 95.896 | 1.713-190.079 | 178.507 | 9.264 | 1/2 |
| 3000 | 3000 | 3.499 | 335.441 | 3.235-667.647 | 715.555 | 59.772 | 1/2 |
| 4000 | 4000 | 0.322 | 14.599 | 14.599-14.599 | 39.007 | 3.671 | 1/1 |
| 5000 | 5000 | 0.275 | 7.975 | 7.975-7.975 | 21.407 | 1.459 | 1/1 |
| 6000 | 6003 | 0.296 | 5.287 | 5.287-5.287 | 21.759 | 1.186 | 1/1 |
| 7000 | 7000 | 0.291 | 3.509 | 3.509-3.509 | 29.727 | 0.710 | 1/1 |
| 8000 | 8000 | 0.270 | 12.455 | 12.455-12.455 | 50.399 | 1.071 | 1/1 |
| 10000 | 10000 | 0.336 | 53.247 | 53.247-53.247 | 151.295 | 2.989 | 0/1 |
| 12000 | 12000 | 0.382 | 179.327 | 179.327-179.327 | 293.119 | 15.367 | 0/1 |

### sharded-4

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.225 | 11.791 | 4.959-18.623 | 29.019 | 3.018 | 2/2 |
| 1000 | 1000 | 1.132 | 23.998 | 1.470-46.527 | 47.459 | 3.096 | 2/2 |
| 2000 | 2000 | 1.990 | 76.343 | 13.807-138.879 | 126.543 | 13.425 | 1/2 |
| 3000 | 3000 | 2.031 | 286.335 | 24.319-548.351 | 499.103 | 41.664 | 1/2 |
| 4000 | 4000 | 0.888 | 72.511 | 72.511-72.511 | 149.247 | 6.423 | 0/1 |
| 5000 | 5000 | 0.890 | 40.063 | 40.063-40.063 | 91.647 | 3.075 | 1/1 |
| 6000 | 5376 | 0.709 | 5783.551 | 5783.551-5783.551 | 5906.431 | 2328.575 | 0/1 |
| 7000 | 2879 | 16031.743 | 30867.455 | 30867.455-30867.455 | 31096.831 | 28639.231 | 0/1 |

### sharded-16

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.371 | 35.747 | 14.791-56.703 | 80.367 | 11.090 | 1/2 |
| 1000 | 1000 | 1.050 | 6.815 | 4.207-9.423 | 23.015 | 2.183 | 2/2 |
| 2000 | 2000 | 0.869 | 9.185 | 8.059-10.311 | 28.719 | 2.179 | 2/2 |
| 3000 | 3000 | 0.956 | 23.703 | 10.671-36.735 | 60.031 | 3.031 | 2/2 |
| 4000 | 4000 | 4.631 | 267.775 | 118.527-417.023 | 544.703 | 19.517 | 0/2 |
| 5000 | 5000 | 2.059 | 198.399 | 133.631-263.167 | 355.647 | 17.193 | 0/2 |

### sharded-64

| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |
|---|---|---|---|---|---|---|---|
| 500 | 500 | 1.813 | 73.779 | 12.647-134.911 | 138.895 | 6.739 | 1/2 |
| 1000 | 1000 | 1.361 | 50.527 | 24.511-76.543 | 110.127 | 8.345 | 1/2 |
| 2000 | 2000 | 1.790 | 84.927 | 84.927-84.927 | 177.663 | 21.167 | 0/1 |
| 3000 | 3001 | 5.095 | 615.423 | 615.423-615.423 | 820.735 | 281.087 | 0/1 |

## Single-request cost in process (real model, real request stream)

`go test -bench` in internal/service with RISKGATE_BENCH_MODEL and RISKGATE_BENCH_REQUESTS; see results/exp5/bench.txt.

| Benchmark | Runs | ns/op median | min | max |
|---|---|---|---|---|
| BenchmarkAssessPipeline | 5 | 188939 | 177753 | 197425 |
| BenchmarkAssessHandler | 5 | 286452 | 214819 | 296131 |
| BenchmarkAssessHTTP | 5 | 472504 | 372230 | 904954 |
| BenchmarkModel/Score | 5 | 78430 | 73973 | 83384 |
| BenchmarkModel/Contributions | 5 | 87737 | 85178 | 107220 |

