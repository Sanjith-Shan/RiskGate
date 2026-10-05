# Stream experiments

One JSON row per run, appended by `cmd/streamexp`. Every row carries `provenance`: date, commit (and whether the working tree was dirty), Go version, OS, CPU, memory, the Kafka broker, and `load`, which is the whole-machine CPU busy share in the 10 s before the run (`baseline_cpu_pct`) and during it (`run_cpu_pct_mean`, `run_cpu_pct_max`), the load average inside the WSL VM where another project ran its benchmarks, how many of its containers were up, and its benchmark lock if it held one. `quotable` and `note` say which numbers in the row may be quoted.

Machine for every row: AMD Ryzen 3 4300U (4 cores, 4 threads), 16 GB, Windows 11, go1.26.5, Apache Kafka 3.9.1 as one local KRaft broker started by `scripts/kafka_local.sh`. The machine was shared with another project for the whole session. The two projects took turns through a lock, so no two benchmarks overlapped, but that project's 22 containers stayed up, and the background CPU before a run was 10 to 30%, sometimes more. **Correctness counts (mismatches, lost or doubled events) do not depend on load and are quotable. Throughput, latency and restart times are not.**

Row-level files (decision logs, snapshots, process logs) were kept under `build/stream/<run>/`, which git ignores, and are not published: they are the IEEE-CIS data in another shape.

| File | Experiment | Runs |
|---|---|---|
| `k1.jsonl` | Parity: all 590,540 payments through Kafka, against the offline pipeline, the HTTP service's decision log, `riskgate audit`, the snapshot counters, and backtests replayed from the topic | `k1-20261005T100004` |
| `k1-arrival-order.jsonl` | k1's negative control: the same replay with the watermarks off | `k1-arrival-order-20261005T123348` |
| `httplog.jsonl` | The HTTP-path reference for k1: the same 590,540 payments through `riskgate serve`, one request at a time | `httplog-1` |
| `k2.jsonl` | Crashes: pipeline processes killed at random during the full replay | see the file |
| `k3.jsonl` | Lag and decision latency at fixed produce rates | see the file |
| `k4.jsonl` | Rebalance: scale out, scale in and a kill mid-replay | `k4-20261005T113656` |
| `naive.jsonl` | Offline: the features a consumer group partitioned by card would compute | one run |
| `skew.jsonl` | Offline: entity events per aggregator partition, and the hot keys | one run |

The k1 run's binaries were built from commit `12f6b06` plus two changes committed afterwards in `04de85c` (members retry while new topics' metadata propagates, and the driver fails on a stalled run instead of waiting forever); neither touches what the pipeline computes. That is why its row says `git_dirty: true`.

The k1 row's first attempt found nothing to verify: both pipeline processes exited at start because the topics they had just created were not yet in the broker's metadata, and the driver waited forever. That is the change above. Its arrival-order companion run in the same slot was stopped. Neither left a row.
