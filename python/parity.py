"""Write LightGBM's raw score for every row of one month, for cmd/parity.

This is the Python half of the parity check: LightGBM's own
predict(raw_score=True) on the exported feature vectors, written with repr()
so the Go side parses the exact double. cmd/parity recomputes each score with
RiskGate's Go evaluator and counts bit-identical rows.

It reads no labels (isFraud is never loaded), so running it on the test month
does not count as looking at the test month.

    python python/parity.py --export data/export --model-dir models/current \\
        --split test --out results/parity_scores.csv
    go run ./cmd/parity -model models/current -export data/export/export.csv \\
        -scores results/parity_scores.csv
"""

import argparse
import json
from pathlib import Path

import lightgbm as lgb
import numpy as np

import riskgate as rg


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--export", required=True, type=Path)
    ap.add_argument("--model-dir", required=True, type=Path)
    ap.add_argument("--split", default="test", choices=["train", "valid", "test"])
    ap.add_argument("--out", required=True, type=Path)
    args = ap.parse_args()

    feats = json.loads((args.model_dir / "metadata.json").read_text())["feature_names"]
    df = rg.read_export(args.export / "export.csv", feats, splits=[args.split], labels=False)
    booster = lgb.Booster(model_file=str(args.model_dir / "model.txt"))  # the bytes Go reads
    if booster.feature_name() != feats:
        raise SystemExit("model.txt feature names differ from metadata.json")
    raw = booster.predict(df[feats].to_numpy(dtype=np.float64), raw_score=True)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with open(args.out, "w") as f:
        f.write("TransactionID,raw_score\n")
        for tid, s in zip(df["TransactionID"].to_numpy(), raw):
            f.write(f"{int(tid)},{float(s)!r}\n")
    print(f"wrote {len(df)} {args.split} rows to {args.out}")


if __name__ == "__main__":
    main()
