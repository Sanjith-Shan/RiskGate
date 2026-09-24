#!/usr/bin/env bash
# Experiment 2: train/serve parity, proved.
#
#   a) Model parity: LightGBM's own raw score (python/parity.py, reads no
#      labels) against the Go evaluator (cmd/parity) on every test-month row.
#   b) Feature parity through the HTTP service: a fresh `riskgate serve`
#      (models/ieee, rules/default.rules, the real backtest table, no
#      snapshot) receives all 590,540 IEEE-CIS transactions, one request at a
#      time in (TransactionDT, TransactionID) order, so its velocity state sees
#      the history the offline export saw. Every logged feature vector is
#      compared bit for bit with a fresh offline replay and with export.csv,
#      and every logged raw score / risk_score with the offline Scorer
#      (cmd/serveparity compare).
#   c) `riskgate audit` over the resulting decision log.
#
# Usage: scripts/experiments/exp2.sh          (from anywhere; ~15-30 min)
#
# Inputs:  data/raw (IEEE-CIS), data/export_real (cmd/export), models/ieee.
# Outputs: results/exp2/{exp2.json,exp2.md,model_parity.json,
#          feature_parity.json,audit.json}. Aggregates only. Row-level files
#          (the full replay, LightGBM's per-row scores, the decision log) stay
#          under data/exp2 and var/exp2, which git ignores.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$PWD

MODEL=${MODEL:-models/ieee}
EXPORT=${EXPORT:-data/export_real}
PORT=${PORT:-18181}
PY=${PY:-.venv/bin/python}
OUT=results/exp2
WORK=data/exp2
VAR=var/exp2
mkdir -p "$OUT" "$WORK" bin
# The service refuses to start without a webhook signing secret; these runs
# send no webhooks, so a random throwaway one is enough.
export RISKGATE_WEBHOOK_SECRETS=${RISKGATE_WEBHOOK_SECRETS:-whsec_exp_$(openssl rand -hex 16)}

for f in "$MODEL/model.txt" "$MODEL/calibration.json" "$EXPORT/export.csv"; do
  [[ -f $f ]] || { echo "exp2: missing $f" >&2; exit 2; }
done

loadavg() { sysctl -n vm.loadavg 2>/dev/null | tr -d '{}' | xargs || uptime; }
START_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LOAD_START=$(loadavg)
COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown)

echo "== build"
go build -o bin/ ./cmd/riskgate ./cmd/parity ./cmd/serveparity

echo "== a) model parity on the test month"
"$PY" python/parity.py --export "$EXPORT" --model-dir "$MODEL" --split test --out "$WORK/lgb_raw_test.csv"
set +e
bin/parity -model "$MODEL" -export "$EXPORT/export.csv" -scores "$WORK/lgb_raw_test.csv" -json "$OUT/model_parity.json"
PARITY_RC=$?
set -e
LOAD_A=$(loadavg)

echo "== b) feature parity through the HTTP service"
TABLE=data/cache/table_ieee_exp2.rgt
if [[ ! -f $TABLE || $TABLE -ot $MODEL/model.txt ]]; then
  bin/riskgate table -data data -model "$MODEL" -out "$TABLE"
fi
[[ -f data/replay_all.jsonl ]] || bin/serveparity replayfile -data data -out data/replay_all.jsonl

rm -rf "$VAR"
mkdir -p "$VAR"
bin/riskgate serve -addr "127.0.0.1:$PORT" -model "$MODEL" -rules rules/default.rules -lists rules/lists.json \
  -table "$TABLE" -snapshot-dir "$VAR" -snapshot-interval 0 -log-level warn 2>"$VAR/serve.log" &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT
for _ in $(seq 1 120); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && break
  sleep 1
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null || { echo "exp2: server did not start" >&2; cat "$VAR/serve.log" >&2; exit 2; }
curl -s "http://127.0.0.1:$PORT/v1/info" >"$VAR/info.json" || true

SEND_START=$(date +%s)
bin/serveparity send -input data/replay_all.jsonl -url "http://127.0.0.1:$PORT/v1/assess" | tee "$VAR/send.txt"
SEND_S=$(( $(date +%s) - SEND_START ))
LOAD_B=$(loadavg)
curl -s "http://127.0.0.1:$PORT/metrics" | grep -E '^riskgate_decision_log_(written|dropped|write_errors)_total' >"$VAR/log_metrics.txt" || true
kill -TERM $SERVER
wait $SERVER || true
trap - EXIT

set +e
bin/serveparity compare -log "$VAR/decisions.jsonl" -data data -export "$EXPORT" -model "$MODEL" \
  -json "$OUT/feature_parity.json" | tee "$VAR/compare.txt"
COMPARE_RC=${PIPESTATUS[0]}

echo "== c) audit"
bin/riskgate audit -log "$VAR/decisions.jsonl" -rules-history "$VAR/rules-history" -model "$MODEL" -json >"$VAR/audit_full.json"
AUDIT_RC=$?
set -e
# Keep aggregates only: the audit's examples would name individual payments.
jq '{records, verified, unscored, versions, mismatched_records, example_count: (.examples | length)}' "$VAR/audit_full.json" >"$OUT/audit.json"

END_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LOAD_END=$(loadavg)

export START_DATE END_DATE LOAD_START LOAD_A LOAD_B LOAD_END COMMIT MODEL EXPORT PARITY_RC COMPARE_RC AUDIT_RC SEND_S VAR OUT TABLE
"$PY" - <<'EOF'
import json, os, platform, subprocess
e = os.environ
def sh(*a):
    try: return subprocess.check_output(a, text=True).strip()
    except Exception: return ""
def sha(p):
    return sh("shasum", "-a", "256", p).split(" ")[0]
out = e["OUT"]; var = e["VAR"]
mp = json.load(open(f"{out}/model_parity.json"))
fp = json.load(open(f"{out}/feature_parity.json"))
au = json.load(open(f"{out}/audit.json"))
metrics = dict(l.split() for l in open(f"{var}/log_metrics.txt") if l.strip())
send = open(f"{var}/send.txt").read().strip()
machine = {
    "cpu": sh("sysctl", "-n", "machdep.cpu.brand_string"),
    "performance_cores": sh("sysctl", "-n", "hw.perflevel0.physicalcpu"),
    "efficiency_cores": sh("sysctl", "-n", "hw.perflevel1.physicalcpu"),
    "logical_cpus": sh("sysctl", "-n", "hw.ncpu"),
    "os": platform.platform(),
    "go_version": sh("go", "env", "GOVERSION"),
    "gomaxprocs": "default (= logical CPUs) for every process",
}
meta = json.load(open(os.path.join(e["MODEL"], "metadata.json")))
doc = {
    "experiment": 2,
    "title": "Train/serve parity",
    "command": "scripts/experiments/exp2.sh",
    "git_commit": e["COMMIT"],
    "started": e["START_DATE"], "finished": e["END_DATE"],
    "machine": machine,
    "load_average": {"start": e["LOAD_START"], "after_model_parity": e["LOAD_A"],
                      "after_service_replay": e["LOAD_B"], "end": e["LOAD_END"]},
    "data_provenance": {
        "dataset": "IEEE-CIS Fraud Detection (Kaggle), real Vesta e-commerce transactions, train_transaction.csv + train_identity.csv",
        "export": e["EXPORT"], "export_csv_sha256": sha(os.path.join(e["EXPORT"], "export.csv")),
        "model": e["MODEL"], "model_txt_sha256": sha(os.path.join(e["MODEL"], "model.txt")),
        "model_trees": mp.get("trees"), "model_trained_at": meta.get("trained_at"),
        "backtest_table": e["TABLE"],
        "labels": "not read by any step (parity.py reads no labels; the service replay omits is_fraud)",
    },
    "a_model_parity": {**mp, "exit_code": int(e["PARITY_RC"])},
    "b_feature_parity": {**fp, "exit_code": int(e["COMPARE_RC"]),
                          "send": send, "send_seconds": int(e["SEND_S"]),
                          "decision_log_metrics": metrics},
    "c_audit": {**au, "exit_code": int(e["AUDIT_RC"])},
}
json.dump(doc, open(f"{out}/exp2.json", "w"), indent=2)

t = fp["by_split"].get("test", {})
a = fp["all"]
def row(name, s):
    return (f"| {name} | {s['rows_compared']:,} | {s['feature_rows_differ']} | {s['encoded_rows_differ_from_export_csv']} | "
            f"{s['raw_score_differ']} | {s['probability_differ']} | {s['risk_score_differ']} | {s['max_abs_feature_diff']:g} | {s['max_abs_raw_score_diff']:g} |")
md = f"""# Experiment 2: train/serve parity

Command `scripts/experiments/exp2.sh`, git commit `{e['COMMIT']}`, {e['START_DATE']} to {e['END_DATE']}.

Machine: {machine['cpu']} ({machine['performance_cores']}P+{machine['efficiency_cores']}E cores, {machine['logical_cpus']} logical CPUs), {machine['go_version']}, GOMAXPROCS default ({machine['logical_cpus']}).
Load average (1/5/15 min): start {e['LOAD_START']}; after the service replay {e['LOAD_B']}; end {e['LOAD_END']}. The machine was shared.
Parity results do not depend on load; only the replay's wall time does.

Data: real IEEE-CIS transactions (all 590,540, test month = month 5), export `{e['EXPORT']}` (export.csv sha256 `{doc['data_provenance']['export_csv_sha256'][:16]}...`),
model `{e['MODEL']}` ({mp.get('trees')} trees, model.txt sha256 `{doc['data_provenance']['model_txt_sha256'][:16]}...`). No step reads labels.

## a) Go evaluator against LightGBM, every test-month row

| Rows checked | Bit-identical | Max abs diff |
|---|---|---|
| {mp['rows_checked']:,} | {mp['bit_identical']:,} | {mp['max_abs_diff']:g} |

## b) Service features against the offline export, every row

A fresh `riskgate serve` (no snapshot, exact state, 64 shards, rules/default.rules) received all {fp['offline_rows']:,} transactions
sequentially in (TransactionDT, TransactionID) order over HTTP ({send}).
Each decision-log line was compared with a fresh offline replay (all {fp['catalog_fields_compared']} catalog fields, bits),
with export.csv (all {fp['model_inputs_compared']} encoded model inputs, bits), and with the offline Scorer (raw score bits, probability bits, risk_score).
Missing on both sides counts as equal.

Decision log: {metrics}. Log lines {fp['decision_log_lines']:,}; offline rows missing from the log {fp['offline_rows_missing_from_log']}; unmatched log lines {fp['log_lines_not_matched']}.

| Rows | Compared | Feature rows differ | Encoded vs export.csv differ | Raw score differ | Probability differ | risk_score differ | Max abs feature diff | Max abs raw diff |
|---|---|---|---|---|---|---|---|---|
{row('All months', a)}
{row('Test month', t)}

Field-level mismatches: {fp['field_mismatch_rows'] or 'none'}.

## c) `riskgate audit` on the decision log

{au['records']:,} decisions, {au['verified']:,} replayed identically, {au['mismatched_records']} mismatched, rule-set versions {au['versions']}.

Verdict: {'PASS, zero mismatches in every check' if (mp['rows_checked']==mp['bit_identical'] and fp['ok'] and au['mismatched_records']==0) else 'MISMATCHES, see BUGS.md'}.
"""
open(f"{out}/exp2.md", "w").write(md)
print(md)
EOF

echo "== negative control"
scripts/experiments/exp2_negative_control.sh
