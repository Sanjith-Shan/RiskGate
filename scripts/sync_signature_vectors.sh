#!/usr/bin/env bash
# Copies Clearinghouse's signature test vectors into RiskGate so CI verifies the
# Go verifier against the exact file the Ruby verifier passes.
#
# Usage: scripts/sync_signature_vectors.sh [path/to/Clearinghouse]
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
clearinghouse="${1:-$HOME/Documents/Clearinghouse}"
src="$clearinghouse/spec/fixtures/signatures.json"
dst="$repo_root/internal/webhook/testdata/clearinghouse_signatures.json"

if [[ ! -f "$src" ]]; then
  echo "no vector file at $src" >&2
  exit 1
fi
python3 -c 'import json, sys; json.load(open(sys.argv[1]))' "$src" || {
  echo "$src is not valid JSON" >&2
  exit 1
}
cp "$src" "$dst"
echo "copied $(python3 -c 'import json, sys; print(len(json.load(open(sys.argv[1]))))' "$dst") vectors to ${dst#"$repo_root"/}"
(cd "$repo_root" && go test ./internal/webhook -run 'TestClearinghouseSignatureVectors' -count=1)
