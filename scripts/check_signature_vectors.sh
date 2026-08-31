#!/usr/bin/env bash
# Cross-checks a signature vector file against an implementation that shares
# no code with internal/webhook: the header grammar is re-implemented below in
# Python and every HMAC is computed by the openssl CLI.
#
# Usage: scripts/check_signature_vectors.sh [vectors.json]
set -euo pipefail

file="${1:-internal/webhook/testdata/signatures.json}"
command -v openssl >/dev/null || { echo "openssl not found" >&2; exit 1; }

python3 - "$file" <<'PY'
import json, re, subprocess, sys

def hmac_hex(secret: bytes, msg: bytes) -> str:
    # openssl takes the key as an argv string; the vectors' secrets are UTF-8 text.
    out = subprocess.run(
        ["openssl", "dgst", "-sha256", "-hmac", secret.decode("utf-8")],
        input=msg, capture_output=True, check=True,
    ).stdout.decode()
    return out.strip().split("= ")[-1].lower()

def verify(v):
    header = v["header"]
    t, sigs = None, []
    for item in header.split(","):
        item = item.strip(" \t")
        if "=" not in item:
            return "malformed_header"
        key, val = item.split("=", 1)
        if key == "":
            return "malformed_header"
        if key == "t":
            if t is not None or not re.fullmatch(r"[0-9]{1,15}", val):
                return "malformed_header"
            t = val
        elif key == "v1":
            if val == "":
                return "malformed_header"
            sigs.append(val)
    if t is None or not sigs:
        return "malformed_header"
    # t is signed exactly as sent; v1 candidates compare as strings against
    # lowercase hex, so uppercase or junk candidates simply do not match.
    msg = t.encode() + b"." + v["payload"].encode("utf-8")
    expected = {hmac_hex(s.encode("utf-8"), msg) for s in v["secrets"]}
    if not expected.intersection(sigs):
        return "no_matching_signature"
    if abs(v["now"] - int(t)) > v["tolerance"]:
        return "timestamp_outside_tolerance"
    return None

vectors = json.load(open(sys.argv[1], encoding="utf-8"))
failures = 0
for v in vectors:
    got = verify(v)
    if (got is None) != v["valid"] or got != v["error"]:
        failures += 1
        print(f"FAIL {v['name']!r}: want {v['error']}, got {got}")
print(f"{len(vectors) - failures}/{len(vectors)} vectors agree with the openssl reference")
sys.exit(1 if failures else 0)
PY
