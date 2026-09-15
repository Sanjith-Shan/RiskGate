#!/usr/bin/env bash
# Experiment 8 warm start. Replays every pre-test payment (months 0-4, in
# (DT, ID) order) through RiskGate and shuts it down gracefully, leaving a
# snapshot of the velocity state as of the start of the test month in
# var/exp8_warm/. Each experiment 8 policy run starts from its own copy, so
# the test month is scored with the history the offline pipeline had, not an
# empty state. Row-level files are deleted when it finishes.
set -euo pipefail
cd "$(dirname "$0")/.."
bg() { taskpolicy -b nice -n 19 env GOMAXPROCS=2 "$@"; }
bg go run ./cmd/serveparity replayfile -out data/replay_all.jsonl >/dev/null
python3 - <<'PY'
import json
test=set(json.loads(l)["payment_id"] for l in open("data/export_real/test_replay.jsonl"))
n=0
with open("data/replay_pretest.jsonl","w") as out:
    for l in open("data/replay_all.jsonl"):
        if json.loads(l)["payment_id"] not in test:
            out.write(l); n+=1
print("pretest rows", n, "test rows", len(test))
PY
rm -f data/replay_all.jsonl
go build -o var/riskgate-warm ./cmd/riskgate
rm -rf var/exp8_warm
# No function wrapper here: $! must be RiskGate itself, so SIGTERM reaches it and it
# writes the final snapshot. taskpolicy, nice and env all exec in place.
RISKGATE_WEBHOOK_SECRETS=whsec_warm_local taskpolicy -b nice -n 19 env GOMAXPROCS=2 var/riskgate-warm -addr 127.0.0.1:8093 -model models/ieee \
  -rules rules/default.rules -lists rules/lists.json -snapshot-dir var/exp8_warm -snapshot-interval 0 \
  -decision-log off > var/exp8_warm.log 2>&1 &
pid=$!
until curl -fsS 127.0.0.1:8093/healthz >/dev/null 2>&1; do sleep 0.2; done
start=$(date +%s)
bg go run ./cmd/serveparity send -url http://127.0.0.1:8093/v1/assess -input data/replay_pretest.jsonl | tail -2
echo "send took $(( $(date +%s) - start ))s"
curl -s 127.0.0.1:8093/v1/info | python3 -c 'import json,sys; d=json.load(sys.stdin); print("model_sha256", d.get("model_sha256") or d.get("model",{}))' || true
kill -TERM $pid; wait $pid || true
rm -f data/replay_pretest.jsonl var/riskgate-warm
ls -la var/exp8_warm; tail -3 var/exp8_warm.log
