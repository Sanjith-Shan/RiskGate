#!/usr/bin/env bash
# Experiment 2 negative control: shows the feature-parity check can fail.
# Takes the decision log exp2.sh produced, changes the first row's amount by
# one ulp (68.5 -> 68.50000000000001) and deletes line 100,001, then runs
# `serveparity compare` on the copy. The check must report exactly one
# differing feature row (field amount) and exactly one row missing from the
# log. Writes results/exp2/negative_control.json (aggregates only) and
# appends a section to results/exp2/exp2.md. Called by exp2.sh.
set -euo pipefail
cd "$(dirname "$0")/../.."
MODEL=${MODEL:-models/ieee}
EXPORT=${EXPORT:-data/export_real}
LOG=var/exp2/decisions.jsonl
NEG=data/exp2/negative_control.jsonl
OUT=results/exp2

first=$(head -1 "$LOG")
[[ $first == *'"features":[68.5,'* ]] || { echo "negative control: first log row is not the expected transaction" >&2; exit 2; }
{
  head -1 "$LOG" | sed 's/"features":\[68\.5,/"features":[68.50000000000001,/'
  sed -n '2,100000p;100002,$p' "$LOG"
} >"$NEG"

set +e
bin/serveparity compare -log "$NEG" -data data -export "$EXPORT" -model "$MODEL" -examples 0 \
  -json var/exp2/negative_control_full.json >var/exp2/negative_control.txt
rc=$?
set -e
rm -f "$NEG"
jq '{rows_compared: .all.rows_compared, feature_rows_differ: .all.feature_rows_differ,
     encoded_rows_differ: .all.encoded_rows_differ_from_export_csv, raw_score_differ: .all.raw_score_differ,
     missing_from_log: .offline_rows_missing_from_log, field_mismatch_rows: .field_mismatch_rows,
     max_abs_feature_diff: .all.max_abs_feature_diff}' var/exp2/negative_control_full.json >"$OUT/negative_control.json"
detected=$(jq -r 'if .feature_rows_differ == 1 and .missing_from_log == 1 and .field_mismatch_rows.amount == 1 then "yes" else "NO" end' "$OUT/negative_control.json")
jq --slurpfile n "$OUT/negative_control.json" --arg rc "$rc" --arg d "$detected" \
  '.negative_control = ($n[0] + {compare_exit_code: ($rc|tonumber), both_faults_detected: $d})' \
  "$OUT/exp2.json" >"$OUT/exp2.json.tmp" && mv "$OUT/exp2.json.tmp" "$OUT/exp2.json"
cat >>"$OUT/exp2.md" <<EOF

## Negative control

To show the check can fail, a copy of the decision log had the first row's amount moved by one ulp
(68.5 to 68.50000000000001) and one line deleted. \`serveparity compare\` on the copy reported
$(jq -r '"\(.feature_rows_differ) differing feature row (fields \(.field_mismatch_rows|keys|join(", "))), \(.encoded_rows_differ) differing encoded row, max abs feature diff \(.max_abs_feature_diff), \(.missing_from_log) row missing from the log"' "$OUT/negative_control.json"),
exit code $rc. Both faults detected: $detected.
EOF
echo "negative control: both faults detected: $detected (compare exit $rc)"
[[ $detected == yes ]]
