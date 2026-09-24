#!/usr/bin/env bash
# Experiment 5: latency under load.
#
# For each state-concurrency configuration (sharded with 1, 4, 16 and 64
# shards, one mutex, sync.Map), start a fresh `riskgate serve` with
# models/ieee, rules/default.rules + rules/lists.json and the real backtest
# table, then drive POST /v1/assess with cmd/loadgen: open loop, one fixed
# arrival rate at a time, latency from each request's intended send time
# (HdrHistogram), deadline 50 ms. Rates climb until two consecutive rates miss
# the deadline (p99 over it, or any error/timeout/non-2xx). The whole sweep is
# repeated REPS times. Then the in-process benchmarks of the assess path run
# against the real model and request stream.
#
# Usage: scripts/experiments/exp5.sh
#   env: REPS=3 DURATION=20s WARMUP=5s DEADLINE=50ms
#        RATES="500 1000 2000 ..."   CONFIGS="sharded:1 sharded:4 ... locked syncmap"
#        SERVER_PROCS= LOADGEN_PROCS=   (GOMAXPROCS for each process; empty = all CPUs)
#        SKIP_BENCH=1                   (skip the go test benchmarks)
#
# Server and load generator share the machine; the result says so. Run it on
# a quiet machine before quoting any number.
#
# Outputs: results/exp5/{exp5.json,exp5.md,bench.txt}; per-rate loadgen JSON
# under var/exp5 (aggregates, no payment data).
set -euo pipefail
cd "$(dirname "$0")/../.."

MODEL=${MODEL:-models/ieee}
EXPORT=${EXPORT:-data/export_real}
PORT=${PORT:-18282}
PY=${PY:-.venv/bin/python}
REPS=${REPS:-3}
DURATION=${DURATION:-20s}
WARMUP=${WARMUP:-5s}
DEADLINE=${DEADLINE:-50ms}
RATES=${RATES:-"500 1000 2000 3000 4000 5000 6000 7000 8000 10000 12000 14000 16000 20000 25000 30000"}
CONFIGS=${CONFIGS:-"sharded:1 sharded:4 sharded:16 sharded:64 locked syncmap"}
SERVER_PROCS=${SERVER_PROCS:-}
LOADGEN_PROCS=${LOADGEN_PROCS:-}
OUT=results/exp5
VAR=var/exp5
mkdir -p "$OUT" "$VAR" data/exp5 bin
# The service refuses to start without a webhook signing secret; these runs
# send no webhooks, so a random throwaway one is enough.
export RISKGATE_WEBHOOK_SECRETS=${RISKGATE_WEBHOOK_SECRETS:-whsec_exp_$(openssl rand -hex 16)}

for f in "$MODEL/model.txt" "$MODEL/calibration.json" "$EXPORT/test_replay.jsonl"; do
  [[ -f $f ]] || { echo "exp5: missing $f" >&2; exit 2; }
done

loadavg() { sysctl -n vm.loadavg 2>/dev/null | tr -d '{}' | xargs || uptime; }
START_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUNDIR=$VAR/$RUN_ID
mkdir -p "$RUNDIR"

go build -o bin/ ./cmd/riskgate ./cmd/loadgen

# The request stream: the test month's replay events without the label
# (the service ignores unknown top-level keys, but it should never see one).
REQS=data/exp5/requests.jsonl
sed 's/,"is_fraud":[01]}$/}/' "$EXPORT/test_replay.jsonl" >"$REQS"

TABLE=${TABLE:-data/cache/table_scored_real.rgt}
if [[ ! -f $TABLE || $TABLE -ot $MODEL/model.txt ]]; then
  bin/riskgate table -data data -model "$MODEL" -out "$TABLE"
fi

deadline_ms=$(echo "$DEADLINE" | sed 's/ms$//')
SERVER=
cleanup() { [[ -n $SERVER ]] && kill "$SERVER" 2>/dev/null || true; }
trap cleanup EXIT

for rep in $(seq 1 "$REPS"); do
  for cfg in $CONFIGS; do
    mode=${cfg%%:*}
    shards=64
    [[ $cfg == *:* ]] && shards=${cfg##*:}
    name=$mode; [[ $mode == sharded ]] && name="sharded-$shards"
    dir=$RUNDIR/rep$rep/$name
    snap=$VAR/state_$name
    rm -rf "$snap"; mkdir -p "$dir" "$snap"
    echo "== rep $rep $name ($(date -u +%H:%M:%S), load $(loadavg))"
    GOMAXPROCS=$SERVER_PROCS bin/riskgate serve -addr "127.0.0.1:$PORT" -model "$MODEL" \
      -rules rules/default.rules -lists rules/lists.json -table "$TABLE" \
      -sharding "$mode" -shards "$shards" -snapshot-dir "$snap" -snapshot-interval 0 -log-level warn \
      2>"$dir/serve.log" &
    SERVER=$!
    for _ in $(seq 1 120); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && break; sleep 1; done
    fails=0
    for rate in $RATES; do
      la=$(loadavg)
      set +e
      GOMAXPROCS=$LOADGEN_PROCS bin/loadgen -url "http://127.0.0.1:$PORT/v1/assess" -input "$REQS" \
        -rate "$rate" -duration "$DURATION" -warmup "$WARMUP" -deadline "$DEADLINE" -json \
        >"$dir/rate_$rate.json" 2>"$dir/rate_$rate.err"
      set -e
      echo "$la" >"$dir/rate_$rate.load"
      if jq -e --argjson d "$deadline_ms" \
          '.runs[0] | (.errors == 0 and .timeouts == 0 and .non_2xx == 0 and .latency_ms.p99 <= $d)' \
          "$dir/rate_$rate.json" >/dev/null 2>&1; then
        fails=0; verdict=inside
      else
        fails=$((fails + 1)); verdict=OUTSIDE
      fi
      echo "   $rate rps: $(jq -r '.runs[0] | "achieved \(.achieved_rps|floor) p50 \(.latency_ms.p50) p99 \(.latency_ms.p99) p99.9 \(.latency_ms.p99_9) ms, err \(.errors) to \(.timeouts)"' "$dir/rate_$rate.json" 2>/dev/null || echo failed) [$verdict] load $la"
      [[ $fails -ge 2 ]] && break
    done
    curl -s "http://127.0.0.1:$PORT/metrics" | grep -E '^riskgate_(decision_log_dropped_total|velocity_|deadline)' >"$dir/metrics.txt" || true
    kill -TERM "$SERVER"; wait "$SERVER" || true; SERVER=
    rm -rf "$snap" # the decision log is GBs at these rates; latency is what this run keeps
  done
done

if [[ -z ${SKIP_BENCH:-} ]]; then
  echo "== in-process benchmarks, real model and request stream (load $(loadavg))"
  BENCH_LOAD=$(loadavg)
  RISKGATE_BENCH_MODEL=$PWD/$MODEL RISKGATE_BENCH_REQUESTS=$PWD/$REQS \
    go test -run '^$' -bench 'AssessPipeline|AssessHandler|AssessHTTP|Model' -benchmem -count 5 ./internal/service \
    | tee "$OUT/bench.txt"
  echo "load average during benchmarks: start $BENCH_LOAD, end $(loadavg)" >>"$OUT/bench.txt"
fi

END_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
export START_DATE END_DATE COMMIT RUNDIR OUT MODEL REPS DURATION WARMUP DEADLINE RATES CONFIGS SERVER_PROCS LOADGEN_PROCS deadline_ms
"$PY" scripts/experiments/exp5_report.py
