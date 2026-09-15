#!/usr/bin/env bash
# Experiment 9: the risk service fails.
#
# Clearinghouse's runner (bin/exp-risk-failure in ~/Documents/Clearinghouse)
# drives an open-loop stream of risk checks through Clearinghouse's RiskGate
# client while it SIGKILLs RiskGate and starts it again (kill mode), and while
# it SIGSTOPs and resumes it (freeze mode). This script builds RiskGate, points
# the runner at the real binary with the real IEEE-CIS model, and keeps every
# output inside RiskGate's results/ (never Clearinghouse's).
#
# RiskGate snapshots on an interval, so a SIGKILL loses the velocity updates
# since the last snapshot. That loss is part of what this measures; set
# SNAPSHOT_INTERVAL to see how it moves.
#
# Before the runner, the script measures restart-to-ready on its own: start
# RiskGate over an existing snapshot and time until /healthz answers.
#
# Needs a quiet machine. Usage: scripts/experiments/exp9.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
clearinghouse="${CLEARINGHOUSE_DIR:-$HOME/Documents/Clearinghouse}"
out="$root/results/exp9"
work="$root/var/exp9"
rate="${RATE:-200}"
interval="${SNAPSHOT_INTERVAL:-5s}"
replay="${REPLAY:-$root/data/export_real/test_replay.jsonl}"
secret="whsec_exp9_$(openssl rand -hex 8)"

mkdir -p "$out" "$work"
go build -o "$work/riskgate" "$root/cmd/riskgate"

args="-model $root/models/ieee -rules $root/rules/default.rules -lists $root/rules/lists.json"
args="$args -snapshot-dir $work/state -snapshot-interval $interval -decision-log off"

# Restart-to-ready over the warm snapshot (velocity state as of the start of
# the test month, from scripts/experiments/exp8_warm.sh): copy it, start
# RiskGate over it, and time until /healthz answers. The failure runs below
# then start from the same state.
port=18089
[ -f "$root/var/exp8_warm/riskgate.snap" ] || "$root/scripts/experiments/exp8_warm.sh"
rm -rf "$work/state"; cp -R "$root/var/exp8_warm" "$work/state"
snapshot_bytes=$(stat -f%z "$work/state/riskgate.snap")
start=$(python3 -c 'import time; print(time.time())')
"$work/riskgate" $args -addr "127.0.0.1:$port" -webhook-secrets "$secret" >"$work/restart.log" 2>&1 &
pid=$!
until curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; do sleep 0.005; done
ready=$(python3 -c "import time; print(round((time.time()-$start)*1000, 1))")
kill -TERM "$pid"; wait "$pid" || true
rm -rf "$work/state"; cp -R "$root/var/exp8_warm" "$work/state"
printf '{"snapshot_bytes": %s, "restart_to_ready_ms": %s, "snapshot_interval": "%s"}\n' \
  "$snapshot_bytes" "$ready" "$interval" >"$out/restart.json"

# The failure runs. RISKGATE_WEBHOOK_SECRETS reaches the process the runner starts.
cd "$clearinghouse"
RISKGATE_WEBHOOK_SECRETS="$secret" bin/exp-risk-failure --mode both --rate "$rate" \
  --riskgate-bin "$work/riskgate" --riskgate-dir "$root" --riskgate-args "$args" \
  --replay "$replay" --out "$out"

"$root/scripts/experiments/provenance.sh" >"$out/provenance.json" 2>/dev/null || true
echo "results in $out"
