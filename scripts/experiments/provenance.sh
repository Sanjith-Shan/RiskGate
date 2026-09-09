#!/bin/sh
# Prints the provenance block every results/<experiment>/ file carries:
# commit, date, machine, Go version, GOMAXPROCS and machine load, as JSON.
# Usage: scripts/experiments/provenance.sh > results/<exp>/provenance.json
set -eu
cd "$(dirname "$0")/../.."
gomaxprocs=${GOMAXPROCS:-$(sysctl -n hw.ncpu 2>/dev/null || nproc)}
cat <<JSON
{
  "git_commit": "$(git rev-parse HEAD 2>/dev/null || echo unknown)",
  "date_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "cpu": "$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)",
  "performance_cores": "$(sysctl -n hw.perflevel0.physicalcpu 2>/dev/null || echo unknown)",
  "efficiency_cores": "$(sysctl -n hw.perflevel1.physicalcpu 2>/dev/null || echo unknown)",
  "go_version": "$(go env GOVERSION)",
  "gomaxprocs": ${gomaxprocs},
  "uptime": "$(uptime | sed 's/^ *//')",
  "data": "IEEE-CIS Fraud Detection (real, Vesta via Kaggle), data/export_real/features.table"
}
JSON
