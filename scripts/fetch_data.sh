#!/usr/bin/env bash
# Downloads the IEEE-CIS Fraud Detection training files into data/raw.
#
# One-time setup only you can do:
#   1. Create a Kaggle API token (kaggle.com > Settings > API). Any of these
#      works: ~/.kaggle/access_token (Kaggle CLI 2.x), KAGGLE_API_TOKEN,
#      ~/.kaggle/kaggle.json (legacy), or KAGGLE_USERNAME and KAGGLE_KEY.
#   2. Accept the competition rules at
#      https://www.kaggle.com/competitions/ieee-fraud-detection/rules
#
# The data is never committed; data/ is gitignored.
set -euo pipefail

COMPETITION=ieee-fraud-detection
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RAW="$ROOT/data/raw"
FILES=(train_transaction.csv train_identity.csv)

die() { printf 'fetch_data: %s\n' "$*" >&2; exit 1; }

have_all() {
  for f in "${FILES[@]}"; do [[ -s "$RAW/$f" ]] || return 1; done
}

if have_all; then
  echo "fetch_data: already have ${FILES[*]} in $RAW"
  exit 0
fi

KAGGLE="$(command -v kaggle || true)"
[[ -z "$KAGGLE" && -x /opt/anaconda3/bin/kaggle ]] && KAGGLE=/opt/anaconda3/bin/kaggle
[[ -n "$KAGGLE" ]] || die "the kaggle CLI is not installed (pip install kaggle)"

has_credentials() {
  [[ -s "$HOME/.kaggle/access_token" ]] && return 0      # Kaggle CLI 2.x token file
  [[ -n "${KAGGLE_API_TOKEN:-}" ]] && return 0           # Kaggle CLI 2.x token in the environment
  [[ -f "$HOME/.kaggle/kaggle.json" ]] && return 0       # legacy username/key file
  [[ -n "${KAGGLE_USERNAME:-}" && -n "${KAGGLE_KEY:-}" ]] && return 0
  return 1
}
has_credentials || die "no Kaggle credentials. Create an API token at https://www.kaggle.com/settings (API section)
and save it as ~/.kaggle/access_token (or export KAGGLE_API_TOKEN). Legacy ~/.kaggle/kaggle.json
or KAGGLE_USERNAME/KAGGLE_KEY also work. Keep token files chmod 600."
for f in "$HOME/.kaggle/access_token" "$HOME/.kaggle/kaggle.json"; do
  [[ -f "$f" ]] && chmod 600 "$f" 2>/dev/null || true
done

command -v unzip >/dev/null || die "unzip is not installed"
mkdir -p "$RAW"
cd "$RAW"
for f in "${FILES[@]}"; do
  [[ -s "$f" ]] && continue
  echo "fetch_data: downloading $f"
  if ! out="$("$KAGGLE" competitions download -c "$COMPETITION" -f "$f" -p "$RAW" 2>&1)"; then
    printf '%s\n' "$out" >&2
    if grep -qiE '403|forbidden|rules' <<<"$out"; then
      die "Kaggle refused the download. Accept the competition rules first:
https://www.kaggle.com/competitions/$COMPETITION/rules"
    fi
    if grep -qiE '401|unauthorized|authenticate' <<<"$out"; then
      die "Kaggle rejected the credentials; create a fresh API token."
    fi
    die "download of $f failed"
  fi
  # Kaggle serves large files zipped.
  if [[ -f "$f.zip" ]]; then
    unzip -o -q "$f.zip" && rm -f "$f.zip"
  fi
  [[ -s "$f" ]] || die "$f is missing after download"
done

echo "fetch_data: done. Files in $RAW:"
ls -lh "${FILES[@]}"
echo "fetch_data: next, go run ./cmd/export (the first load builds data/cache/ieee.rgc)"
