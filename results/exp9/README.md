# Experiment 9: the risk service fails

Clearinghouse's runner (`bin/exp-risk-failure`) sent an open-loop stream of risk checks at 200/s through Clearinghouse's RiskGate client. The client has a 50 ms deadline, a circuit breaker (opens after 5 consecutive failures, 2 s cool-down) and fails open. Each run had three 10 s phases: healthy, down, healthy again. In **kill** mode RiskGate was SIGKILLed, then a new process started over the last periodic snapshot (interval 5 s). In **freeze** mode it was SIGSTOPped, then SIGCONTed. The real RiskGate ran the IEEE-CIS model and `rules/default.rules`, starting from the warm snapshot (`scripts/experiments/exp8_warm.sh`), which holds velocity state as of the start of the test month. Requests came from the test-month replay. Run by `scripts/experiments/exp9.sh` on 2026-09-24.

| | kill | freeze |
|---|---|---|
| Calls | 6,000 | 6,000 |
| Went through unchecked (failed open) | 3,240 | 2,063 |
| First checked payment after restart or thaw | 2.06 s after restart | 0.32 s after thaw |
| Client p99 over the whole run | 7.4 ms | 5.5 ms |
| Client max latency | 57.3 ms | 59.0 ms |

- **Restart to ready over the warm snapshot** (4.16 MB, 498,113 payments of history): 383 ms until `/healthz` answered (`restart.json`).
- **What failing open costs.** During the 10 s outage every payment went through unchecked: about 2,000 at 200/s, plus the 2 s breaker cool-down after RiskGate was back. The client's latency stayed bounded by the deadline (max 57-59 ms against a 50 ms deadline plus overhead), so Clearinghouse kept taking payments.
- **Freeze is the case the deadline exists for.** Connections hang instead of being refused. Only the first 16 calls paid the full 50 ms before the breaker opened.

**Not yet explained, not cited.** In kill mode the breaker also opened once in each healthy phase, about 6 to 7 s after each RiskGate process started. Each time it caused about 415 unchecked payments. This happened in two runs. Freeze mode never did it. A direct probe ruled out periodic snapshots (turning them off did not help) and GC (every pause under 1 ms). Another project's search jobs were running during both runs (load average 6 to 8, see `provenance.json`), so contention is the leading explanation. A cold-start effect in a freshly restored process is not ruled out. It needs a rerun on an idle machine before any healthy-phase number from kill mode is quoted.

Files: `riskgate-{kill,freeze}.{md,json}` (real RiskGate), `fake-riskgate-*` (Clearinghouse's fake, for comparison), `restart.json`, `provenance.json`. Aggregates only; no payment rows.
