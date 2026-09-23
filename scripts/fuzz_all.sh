#!/usr/bin/env bash
# Runs every native Go fuzz target in the module for a short time.
#
# Targets are discovered by grepping for `func FuzzXxx(` in test files, so a
# fuzz target added to any package is fuzzed in CI without editing this file.
# `go test -fuzz` accepts one package and one target per invocation, hence the
# loop.
#
# Usage: scripts/fuzz_all.sh [fuzztime]    (default 20s, or $FUZZTIME)
set -euo pipefail

fuzztime="${1:-${FUZZTIME:-20s}}"
cd "$(dirname "$0")/.."

targets="$(grep -rEo --include='*_test.go' --exclude-dir=.venv --exclude-dir=vendor --exclude-dir=.git \
  '^func Fuzz[A-Za-z0-9_]*\(' . | sed -E 's/^(.*)\/[^/]*_test\.go:func (Fuzz[A-Za-z0-9_]*)\(/\1 \2/' | sort -u)"

if [[ -z "$targets" ]]; then
  echo "no fuzz targets found"
  exit 0
fi

count=0
failed=()
while read -r dir name; do
  count=$((count + 1))
  echo "::group::$name in $dir ($fuzztime)"
  if ! go test -run='^$' -fuzz="^${name}\$" -fuzztime="$fuzztime" -fuzzminimizetime=2s "$dir"; then
    failed+=("$dir $name")
  fi
  echo "::endgroup::"
done <<< "$targets"

echo "fuzzed $count targets for $fuzztime each"
if (( ${#failed[@]} )); then
  printf 'FAILED: %s\n' "${failed[@]}"
  echo "failing inputs are saved under <package>/testdata/fuzz/<target>/; commit them as regression seeds once fixed"
  exit 1
fi
