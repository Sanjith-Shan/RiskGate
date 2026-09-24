"""Assemble experiment 5's results from the per-rate loadgen JSON files.

Called by scripts/experiments/exp5.sh with its settings in the environment.
Writes results/exp5/exp5.json and results/exp5/exp5.md. Standard library only.
"""

import json
import os
import re
import statistics
import subprocess
from pathlib import Path

E = os.environ
RUNDIR = Path(E["RUNDIR"])
OUT = Path(E["OUT"])
DEADLINE = float(E["deadline_ms"])
ORDER = ["locked", "syncmap", "sharded-1", "sharded-4", "sharded-16", "sharded-64"]


def sh(*a):
    try:
        return subprocess.check_output(a, text=True).strip()
    except Exception:
        return ""


def inside(r):
    return r["errors"] == 0 and r["timeouts"] == 0 and r["non_2xx"] == 0 and r["latency_ms"]["p99"] <= DEADLINE


def main():
    runs = {}  # config -> rate -> [per-rep dict]
    machine = None
    maxrate = {}  # config -> [per-rep max rate inside]
    reps = sorted(p for p in RUNDIR.glob("rep*") if p.is_dir())
    for rep in reps:
        for cdir in sorted(p for p in rep.iterdir() if p.is_dir()):
            cfg = cdir.name
            best = None
            for f in sorted(cdir.glob("rate_*.json"), key=lambda p: float(p.stem.split("_")[1])):
                try:
                    doc = json.loads(f.read_text())
                except ValueError:
                    continue
                machine = machine or doc["machine"]
                r = doc["runs"][0]
                rate = r["target_rps"]
                load = f.with_suffix(".load").read_text().strip()
                ok = inside(r)
                runs.setdefault(cfg, {}).setdefault(rate, []).append({
                    "rep": rep.name, "achieved_rps": r["achieved_rps"], "sent": r["sent"],
                    "ok": r["ok"], "errors": r["errors"], "timeouts": r["timeouts"], "non_2xx": r["non_2xx"],
                    "latency_ms": r["latency_ms"], "send_lag_ms": r["send_lag_ms"],
                    "inside_deadline": ok, "load_average_before": load,
                })
                if ok and (best is None or rate > best):
                    best = rate
            maxrate.setdefault(cfg, []).append(best)
            m = cdir / "metrics.txt"
            if m.exists():
                runs[cfg].setdefault("_metrics", []).append(m.read_text().strip())

    summary = {}
    for cfg, rates in runs.items():
        rows = []
        for rate in sorted(k for k in rates if k != "_metrics"):
            rs = rates[rate]
            med = lambda k: statistics.median(x["latency_ms"][k] for x in rs)
            rows.append({
                "target_rps": rate, "reps": len(rs),
                "achieved_rps_median": statistics.median(x["achieved_rps"] for x in rs),
                "p50_ms_median": med("p50"), "p99_ms_median": med("p99"), "p99_9_ms_median": med("p99_9"),
                "p99_ms_min": min(x["latency_ms"]["p99"] for x in rs),
                "p99_ms_max": max(x["latency_ms"]["p99"] for x in rs),
                "send_lag_p99_ms_median": statistics.median(x["send_lag_ms"]["p99"] for x in rs),
                "reps_inside_deadline": sum(x["inside_deadline"] for x in rs),
            })
        per_rep = maxrate.get(cfg, [])
        summary[cfg] = {
            "max_rate_inside_deadline_per_rep": per_rep,
            "max_rate_inside_deadline_median": statistics.median([x or 0 for x in per_rep]) if per_rep else None,
            "rates": rows,
            "server_metrics_at_end": rates.get("_metrics", []),
        }

    bench = {}
    bt = OUT / "bench.txt"
    if bt.exists():
        for line in bt.read_text().splitlines():
            m = re.match(r"^(Benchmark\S+?)(-\d+)?\s+\d+\s+([\d.]+) ns/op(?:\s+([\d.]+) B/op\s+([\d.]+) allocs/op)?", line)
            if m:
                bench.setdefault(m.group(1), []).append(float(m.group(3)))
        bench = {k: {"runs": len(v), "ns_per_op_median": statistics.median(v), "ns_per_op_min": min(v), "ns_per_op_max": max(v)}
                 for k, v in bench.items()}

    loads = [x["load_average_before"] for c in runs.values() for k, rs in c.items() if k != "_metrics" for x in rs]
    ones = [float(x.split()[0]) for x in loads if x]
    meta = json.loads((Path(E["MODEL"]) / "metadata.json").read_text())
    trees = sh("grep", "-c", "^Tree=", str(Path(E["MODEL"]) / "model.txt"))
    doc = {
        "experiment": 5,
        "title": "Latency under load",
        "command": "scripts/experiments/exp5.sh",
        "settings": {k: E.get(k, "") for k in ["REPS", "DURATION", "WARMUP", "DEADLINE", "RATES", "CONFIGS", "SERVER_PROCS", "LOADGEN_PROCS"]},
        "git_commit": E["COMMIT"], "started": E["START_DATE"], "finished": E["END_DATE"],
        "machine": machine,
        "gomaxprocs": {"server": E.get("SERVER_PROCS") or "default (all logical CPUs)",
                        "loadgen": E.get("LOADGEN_PROCS") or "default (all logical CPUs)"},
        "load_average_1min_before_each_rate": {"min": min(ones) if ones else None, "median": round(statistics.median(ones), 2) if ones else None,
                                                "max": max(ones) if ones else None},
        "shared_machine_warning": "Server and load generator ran on the same machine, which was shared with other heavy jobs. Re-run on a quiet machine before quoting.",
        "data_provenance": {
            "requests": "test month of real IEEE-CIS transactions (data/export_real/test_replay.jsonl, 92,427 lines, is_fraud stripped), sent round-robin with payment_id rewritten unique per send",
            "model": E["MODEL"], "model_trees": int(trees) if trees else None, "model_trained_at": meta.get("trained_at"),
            "rules": "rules/default.rules + rules/lists.json",
            "state": "exact velocity state, fresh at the start of each configuration; it keeps filling across that configuration's rates. After the 92,427-line file wraps, event times repeat and the state records them at each key's latest time (late-event rule).",
        },
        "method": "cmd/loadgen, open loop, one fixed rate per invocation, latency from intended send time, HdrHistogram; a rate is inside the deadline only if p99 <= deadline and there were no errors, timeouts or non-2xx. Rates climb until two consecutive rates miss.",
        "deadline_ms": DEADLINE,
        "configs": summary,
        "benchmarks": bench,
        "raw_runs": runs,
    }
    (OUT / "exp5.json").write_text(json.dumps(doc, indent=2) + "\n")

    mach = machine or {}
    lines = [
        "# Experiment 5: latency under load",
        "",
        f"Command `scripts/experiments/exp5.sh` (REPS={E['REPS']}, DURATION={E['DURATION']}, WARMUP={E['WARMUP']}, deadline {E['DEADLINE']}), "
        f"git commit `{E['COMMIT']}`, {E['START_DATE']} to {E['END_DATE']}.",
        "",
        f"Machine: {mach.get('cpu')} ({mach.get('performance_cores')}P+{mach.get('efficiency_cores')}E cores, {mach.get('logical_cpus')} logical CPUs), "
        f"{mach.get('go_version')}. GOMAXPROCS: server {doc['gomaxprocs']['server']}, loadgen {doc['gomaxprocs']['loadgen']}. "
        "Server and load generator shared the machine.",
        "",
        f"**Load.** 1-minute load average before each rate: min {doc['load_average_1min_before_each_rate']['min']}, "
        f"median {doc['load_average_1min_before_each_rate']['median']}, max {doc['load_average_1min_before_each_rate']['max']} "
        f"(on {mach.get('logical_cpus')} logical CPUs). The machine was shared with other heavy jobs. "
        "**These numbers are not quotable; re-run `scripts/experiments/exp5.sh` on a quiet machine first.**",
        "",
        f"Data: {doc['data_provenance']['requests']}. Model `{E['MODEL']}` ({trees} trees), {doc['data_provenance']['rules']}. "
        f"{doc['data_provenance']['state']}",
        "",
        "## Highest rate with p99 inside the deadline",
        "",
        "| Config | Per repetition (req/s; none = no rate passed) | Median (none counted as 0) |",
        "|---|---|---|",
    ]
    for cfg in [c for c in ORDER if c in summary] + [c for c in summary if c not in ORDER]:
        s = summary[cfg]
        lines.append(f"| {cfg} | {', '.join(str(int(x)) if x else 'none' for x in s['max_rate_inside_deadline_per_rep'])} | "
                     f"{int(s['max_rate_inside_deadline_median'] or 0)} |")
    steps = [x for c in runs.values() for k, rs in c.items() if k != "_metrics" for x in rs]
    lagged = sum(1 for x in steps if x["send_lag_ms"]["p99"] > 1.0)
    doc["generator_lag_steps"] = {"rate_steps": len(steps), "send_lag_p99_over_1ms": lagged}
    (OUT / "exp5.json").write_text(json.dumps(doc, indent=2) + "\n")
    lines += ["", f"**Generator health.** In {lagged} of {len(steps)} rate steps the load generator's own p99 send lag was over 1 ms, "
              "meaning it could not send on schedule. Where that happens the latency includes CPU starvation of the "
              "generator and the server by the rest of the machine, not only RiskGate's cost. "
              "The configurations ran one after another while the load changed, so load and configuration are confounded "
              "and the ranking between configurations is not meaningful from this run."]
    lines += ["", "## Per rate (medians over repetitions; latency in ms from intended send time)", ""]
    for cfg in [c for c in ORDER if c in summary] + [c for c in summary if c not in ORDER]:
        lines += [f"### {cfg}", "", "| Target rps | Achieved | p50 | p99 | p99 range | p99.9 | Send lag p99 | Reps inside |", "|---|---|---|---|---|---|---|---|"]
        for r in summary[cfg]["rates"]:
            lines.append(f"| {int(r['target_rps'])} | {r['achieved_rps_median']:.0f} | {r['p50_ms_median']:.3f} | {r['p99_ms_median']:.3f} | "
                         f"{r['p99_ms_min']:.3f}-{r['p99_ms_max']:.3f} | {r['p99_9_ms_median']:.3f} | {r['send_lag_p99_ms_median']:.3f} | "
                         f"{r['reps_inside_deadline']}/{r['reps']} |")
        lines.append("")
    if bench:
        lines += ["## Single-request cost in process (real model, real request stream)", "",
                  "`go test -bench` in internal/service with RISKGATE_BENCH_MODEL and RISKGATE_BENCH_REQUESTS; see results/exp5/bench.txt.", "",
                  "| Benchmark | Runs | ns/op median | min | max |", "|---|---|---|---|---|"]
        for k, v in bench.items():
            lines.append(f"| {k} | {v['runs']} | {v['ns_per_op_median']:.0f} | {v['ns_per_op_min']:.0f} | {v['ns_per_op_max']:.0f} |")
        lines.append("")
    (OUT / "exp5.md").write_text("\n".join(lines) + "\n")
    print("\n".join(lines))


if __name__ == "__main__":
    main()
