# Load generator

`cmd/loadgen` drives `POST /v1/assess` at fixed arrival rates for experiment 5
(latency under load). It is open-loop and measures latency from each request's
**intended** send time, so a server stall shows up in the percentiles instead of
being hidden by the generator slowing down.

## Why open-loop

A closed-loop tool (N workers, each waiting for its response before sending the
next request) sends less when the server is slow. If the server pauses for one
second, each worker records one slow request and simply never sends the ones it
would have sent during the pause. Those missing samples are exactly the ones that
would have shown the pause, so p99 looks healthy. Gil Tene calls this
*coordinated omission*.

RiskGate sits in Clearinghouse's confirm path, and Clearinghouse's payments
arrive whether or not RiskGate is keeping up. So the generator fixes a timetable
before it starts, with request *i* due at `start + i/rate`, and charges every
request from its due time to the end of its response. A request that could not
be sent on time because every connection was stuck behind a stall waits in the
generator, and that wait counts toward its latency, as it would for a real
payment.

`TestCoordinatedOmission` in `internal/loadgen` checks this. A fake server stalls
for 1 s during a 1000 rps run, and the test requires about 1000 × 1 s requests
with latency above 100 ms (the run records ~930) and a p90 above 300 ms. A
closed-loop tool would show one slow request per worker.

## Keeping the generator from being the bottleneck

- **One goroutine per request, capped by a semaphore** (`-max-inflight`, default
  10000). A single scheduler goroutine sleeps until the next due time, then
  dispatches every request that is due. If the OS timer fires late, the overdue
  requests go out back to back, so the average rate holds.
- **Latency is charged from the intended time even when the generator is late.**
  If the in-flight cap is hit or the scheduler falls behind, the numbers get
  worse, never better.
- **Send lag is reported.** For each request, lag is actual send time minus
  intended send time. A p99 send lag near the server's p99 latency means the
  generator, not the server, is being measured.
- **Tuned transport.** HTTP/1.1 keep-alive with `MaxIdleConnsPerHost` equal to
  the in-flight cap. `net/http`'s default of 2 idle connections per host makes
  almost every request open a new TCP connection at these rates.
- Request bodies are prepared at load time, so rewriting `payment_id` per send
  costs one small allocation.

On the M3 Pro, with the fake server on the same machine, the generator held
50,000 rps with a p99 send lag of 0.5 ms.

## Input

A JSONL file, one assess request per line:

```json
{"payment_id": "pay_123", "created": 1767225600, "amount": 1999, "currency": "usd", "risk_fields": {"card_network": "visa"}}
```

Lines are sent round-robin. With `-unique-ids` (the default), `payment_id` is
rewritten to `<original>_lg<seq>` on every send. RiskGate updates velocity state
on every assess, so replaying one id thousands of times is a different workload.
Rewritten bodies are re-encoded, with keys in sorted order. With
`-unique-ids=false`, each line is sent byte for byte.

## Usage

```sh
go build -o bin/loadgen ./cmd/loadgen

# one rate
bin/loadgen -input reqs.jsonl -rate 2000 -duration 30s

# sweep, stating the deadline, JSON for experiment scripts
bin/loadgen -input reqs.jsonl -rates 1000:20000:1000 -duration 30s -warmup 5s \
  -deadline 50ms -json > results/latency_sweep.json
```

| Flag | Default | Meaning |
|---|---|---|
| `-url` | `http://127.0.0.1:8080/v1/assess` | target |
| `-input` | (required) | JSONL requests |
| `-rate` | 1000 | arrival rate, requests/s |
| `-rates` | | sweep: `a,b,c` or `start:end:step`; overrides `-rate` |
| `-duration` | 30s | measured window per rate |
| `-warmup` | 5s | unrecorded warmup per rate, at that rate |
| `-cooldown` | 2s | pause between sweep steps |
| `-timeout` | 5s | per request, from actual send |
| `-max-inflight` | 10000 | concurrency cap |
| `-unique-ids` | true | rewrite `payment_id` per send |
| `-deadline` | 0 | report the highest rate with p99 inside it |
| `-json` | false | one JSON document on stdout; progress on stderr |

## Output

Text:

```
machine: Apple M3 Pro (6P+6E cores, 12 logical CPUs), darwin/arm64, go1.26.5, GOMAXPROCS=12
target 10000 rps for 10s: sent 100000, achieved 10000.0 rps; ok 100000, non-2xx 0, errors 0, timeouts 0
  latency ms (from intended send): p50 0.078  p90 0.158  p99 0.595  p99.9 10.447  max 17.087
  send lag ms:                     p50 0.022  p99 0.120  max 5.515
  p99 inside the 50ms deadline
```

JSON (`-json`; the values below are illustrative):

```json
{
  "machine": {"cpu": "Apple M3 Pro", "performance_cores": 6, "efficiency_cores": 6,
              "logical_cpus": 12, "os": "darwin", "arch": "arm64",
              "go_version": "go1.26.5", "gomaxprocs": 12},
  "config": {"url": "...", "input": "...", "duration_s": 30, "warmup_s": 5,
             "timeout_ms": 5000, "max_inflight": 10000, "unique_ids": true,
             "deadline_ms": 50, "started_at": "2026-09-23T22:00:00Z"},
  "runs": [
    {"target_rps": 10000, "achieved_rps": 10000.0, "duration_s": 30, "sent": 300000,
     "ok": 300000, "non_2xx": 0, "errors": 0, "timeouts": 0,
     "latency_ms": {"count": 300000, "mean": 0.1, "p50": 0.08, "p90": 0.16,
                    "p99": 0.6, "p99_9": 10.4, "max": 17.1},
     "send_lag_ms": {"count": 300000, "mean": 0.03, "p50": 0.02, "p90": 0.05,
                     "p99": 0.12, "p99_9": 1.2, "max": 5.5}}
  ],
  "max_rate_within_deadline_rps": 10000
}
```

What gets counted:

- `latency_ms` covers 2xx, non-2xx, and timed-out requests. A timeout is
  recorded at the moment it gave up, which is a lower bound. Transport errors
  such as a refused connection are counted in `errors` but kept out of the
  histogram, because failing fast would pull the percentiles down.
- `achieved_rps` is the send rate over the measured window, from the first
  actual send to the last.
- A rate counts as inside the deadline only if its p99 is inside it **and** it
  had no errors, timeouts, or non-2xx responses. Shedding load is not serving it.
- Histograms are HdrHistogram with 3 significant digits over 1 µs to 5 min.

For resume numbers, run the generator and RiskGate on the same labelled machine,
say that they shared it, and record the machine line with the number.
