"""Train RiskGate's model and the baselines it is compared against.

Reads the Go export (cmd/export): export.csv, features.json, encoder.json.
The export already carries the train/valid/test split, a time split by month;
nothing here shuffles or re-splits. Test rows are dropped as the file is read
and never reach this script's memory.

Everything is chosen on the validation month: LightGBM's iteration count
(early stopping), the small hyperparameter grid, logistic regression's C,
the isotonic calibration, and the operating thresholds evaluate.py applies to
the test month. Selection is by validation PR-AUC, since fraud is ~3.5% of
rows and ROC-AUC flatters every model at that base rate.

Baselines, all on the same split:
  rules     The hand-written rule set, scored by RiskGate's Go rule engine,
            not re-implemented here: one implementation of the rules, as of
            the features. Pass its output with --rules-predictions, a CSV of
            TransactionID,flagged (1 if any block or review rule matched).
            Without it the baseline is skipped and the results say so.
  logreg    Logistic regression on all features: signed log1p and median
            imputation with missing-indicators for numbers, standardized;
            one-hot for categoricals (top 19 codes, the rest pooled, missing
            as its own level).
  lgbm_raw  LightGBM on the raw transaction columns only (schema.RawFields).
  lgbm_full LightGBM on raw columns plus RiskGate's velocity features. The
            gap between lgbm_raw and lgbm_full is what the streaming
            features are worth. lgbm_full is the model the service runs.

Writes the service's model directory (model.txt, encoder.json,
calibration.json, metadata.json), the baselines evaluate.py needs, and
validation.json with every run and every choice.

    python python/train.py --export data/export --source ieee-cis \\
        --out models/current --results results
"""

import argparse
import datetime
import itertools
import json
import pickle
import shutil
import sys
from pathlib import Path

import lightgbm as lgb
import numpy as np
import pandas as pd
import sklearn
from sklearn.compose import ColumnTransformer
from sklearn.impute import SimpleImputer
from sklearn.isotonic import IsotonicRegression
from sklearn.linear_model import LogisticRegression
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import FunctionTransformer, OneHotEncoder, StandardScaler

import riskgate as rg

LGB_BASE = {
    "objective": "binary", "learning_rate": 0.05, "feature_fraction": 0.8,
    "bagging_fraction": 0.8, "bagging_freq": 1, "lambda_l2": 1.0,
    "max_cat_to_onehot": 4, "cat_smooth": 10, "min_data_per_group": 50,
    "metric": "average_precision", "seed": 7, "deterministic": True,
    "force_row_wise": True, "verbose": -1,
}
LGB_GRID = {"num_leaves": [31, 63], "min_data_in_leaf": [50, 200]}
MAX_ROUNDS, EARLY_STOP = 3000, 100
LOGREG_C = [0.01, 0.1, 1.0]


def valid_metrics(y, s, amount):
    t_fpr, rec, fpr = rg.threshold_at_fpr(y, s, 0.01)
    t_usd, usd_rec, usd_share = rg.threshold_at_dollar_budget(y, s, amount, 0.01)
    return {
        "roc_auc": rg.roc_auc(y, s), "pr_auc": rg.pr_auc(y, s),
        "recall_at_1pct_fpr": rec, "threshold_1pct_fpr": t_fpr,
        "fraud_dollar_recall_at_1pct_legit_dollars": usd_rec, "threshold_1pct_legit_dollars": t_usd,
    }


def train_lgbm(name, feats, cats, X_tr, y_tr, X_va, y_va, amount_va):
    runs = []
    for leaves, min_leaf in itertools.product(LGB_GRID["num_leaves"], LGB_GRID["min_data_in_leaf"]):
        params = {**LGB_BASE, "num_leaves": leaves, "min_data_in_leaf": min_leaf}
        dtr = lgb.Dataset(X_tr, y_tr, feature_name=feats, categorical_feature=cats, free_raw_data=False)
        dva = lgb.Dataset(X_va, y_va, reference=dtr)
        b = lgb.train(params, dtr, MAX_ROUNDS, valid_sets=[dva],
                      callbacks=[lgb.early_stopping(EARLY_STOP, verbose=False)])
        s = b.predict(X_va, raw_score=True, num_iteration=b.best_iteration)
        m = valid_metrics(y_va, s, amount_va)
        runs.append({"params": params, "best_iteration": b.best_iteration, "valid": m, "_booster": b})
        print(f"  {name} leaves={leaves} min_leaf={min_leaf}: iter={b.best_iteration} "
              f"PR-AUC={m['pr_auc']:.4f} ROC-AUC={m['roc_auc']:.4f}", flush=True)
    best = max(runs, key=lambda r: r["valid"]["pr_auc"])
    return best, runs


def train_logreg(feats, cats, X_tr, y_tr, X_va, y_va, amount_va):
    nums = [f for f in feats if f not in cats]
    signed_log = FunctionTransformer(rg.signed_log1p, feature_names_out="one-to-one")
    pre = ColumnTransformer([
        ("num", make_pipeline(signed_log, SimpleImputer(strategy="median", add_indicator=True),
                              StandardScaler()), nums),
        ("cat", make_pipeline(SimpleImputer(strategy="constant", fill_value=-1),
                              OneHotEncoder(handle_unknown="infrequent_if_exist", max_categories=20)), cats),
    ])
    runs = []
    for C in LOGREG_C:
        model = make_pipeline(pre, LogisticRegression(C=C, max_iter=2000))
        model.fit(pd.DataFrame(X_tr, columns=feats), y_tr)
        s = model.decision_function(pd.DataFrame(X_va, columns=feats))
        m = valid_metrics(y_va, s, amount_va)
        runs.append({"params": {"C": C}, "valid": m, "_model": model})
        print(f"  logreg C={C}: PR-AUC={m['pr_auc']:.4f} ROC-AUC={m['roc_auc']:.4f}", flush=True)
    return max(runs, key=lambda r: r["valid"]["pr_auc"]), runs


def rules_baseline(path, va):
    if path is None:
        return {"skipped": "no --rules-predictions given; score the rule set with the Go rule engine "
                           "and pass its TransactionID,flagged CSV"}
    flags = pd.read_csv(path, dtype={"TransactionID": "int64", "flagged": "int8"})
    merged = va[["TransactionID", "isFraud", "amount"]].merge(flags, on="TransactionID", how="left")
    if merged["flagged"].isna().any():
        raise SystemExit(f"{path}: {merged['flagged'].isna().sum()} validation rows have no rule decision")
    y, f, amt = merged["isFraud"].values, merged["flagged"].values, merged["amount"].values
    rec, fpr = rg.rate_at_threshold(y, f, 1)
    usd_rec, usd_share = rg.dollars_at_threshold(y, f, amt, 1)
    return {"source": str(path), "sha256": rg.fingerprint(path),
            "valid": {"recall": rec, "fpr": fpr, "fraud_dollar_recall": usd_rec,
                      "legit_dollar_share_blocked": usd_share}}


def public(runs):
    return [{k: v for k, v in r.items() if not k.startswith("_")} for r in runs]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--export", required=True, type=Path, help="directory with export.csv, features.json, encoder.json")
    ap.add_argument("--source", required=True, choices=["ieee-cis", "synthetic"],
                    help="where the exported transactions came from; recorded, and synthetic results are labelled")
    ap.add_argument("--out", required=True, type=Path, help="model directory to write")
    ap.add_argument("--results", default=Path("results"), type=Path)
    ap.add_argument("--rules-predictions", type=Path, help="Go rule engine output: TransactionID,flagged")
    args = ap.parse_args()

    csv = args.export / "export.csv"
    feats, cats = rg.load_features(args.export)
    raw_feats = [f for f in feats if f in rg.RAW_FIELDS]
    absent = sorted(set(rg.RAW_FIELDS) - set(raw_feats))
    if absent:
        print(f"note: raw fields not in the export: {absent}", file=sys.stderr)
    if "risk_score" in feats:
        raise SystemExit("features.json lists risk_score, the model's own output")

    df = rg.read_export(csv, feats, splits=["train", "valid"])
    tr, va = df[df["split"] == "train"], df[df["split"] == "valid"]
    if tr["month"].max() >= va["month"].min():
        raise SystemExit("validation month does not follow the training months")
    y_tr, y_va = tr["isFraud"].to_numpy(), va["isFraud"].to_numpy()
    amount_va = va["amount"].to_numpy()
    print(f"train {len(tr)} rows ({y_tr.mean():.2%} fraud), valid {len(va)} rows ({y_va.mean():.2%} fraud)")

    def X(frame, cols):
        return frame[cols].to_numpy(dtype=np.float64)

    print("logistic regression")
    lr, lr_runs = train_logreg(feats, cats, X(tr, feats), y_tr, X(va, feats), y_va, amount_va)
    print("lightgbm, raw columns")
    raw_cats = [c for c in cats if c in raw_feats]
    lraw, lraw_runs = train_lgbm("lgbm_raw", raw_feats, raw_cats, X(tr, raw_feats), y_tr,
                                 X(va, raw_feats), y_va, amount_va)
    print("lightgbm, raw + velocity")
    full, full_runs = train_lgbm("lgbm_full", feats, cats, X(tr, feats), y_tr, X(va, feats), y_va, amount_va)

    # Isotonic calibration of the served model on validation raw scores.
    booster = full["_booster"]
    s_va = booster.predict(X(va, feats), raw_score=True, num_iteration=full["best_iteration"])
    iso = IsotonicRegression(out_of_bounds="clip", y_min=0.0, y_max=1.0).fit(s_va, y_va)
    xs, ys = iso.X_thresholds_.astype(np.float64), iso.y_thresholds_.astype(np.float64)
    p_va = rg.interpolate(s_va, xs, ys)
    assert np.max(np.abs(p_va - iso.predict(s_va))) < 1e-12, "calibration map disagrees with sklearn"

    # Model directory for the Go service.
    args.out.mkdir(parents=True, exist_ok=True)
    booster.save_model(str(args.out / "model.txt"), num_iteration=full["best_iteration"])
    shutil.copyfile(args.export / "encoder.json", args.out / "encoder.json")
    (args.out / "calibration.json").write_text(json.dumps(
        {"method": "isotonic", "input": "raw_score", "x": xs.tolist(), "y": ys.tolist()}, indent=1))
    reloaded = lgb.Booster(model_file=str(args.out / "model.txt"))
    assert np.array_equal(reloaded.predict(X(va, feats), raw_score=True), s_va), "saved model scores differ"

    nums = [f for f in feats if f not in cats]
    stats = {f: {"p50": float(np.nanmedian(tr[f]))} for f in nums if tr[f].notna().any()}
    meta = {
        "model": "lightgbm", "objective": "binary",
        "feature_names": feats, "categorical": cats,
        "params": {k: v for k, v in full["params"].items()}, "best_iteration": full["best_iteration"],
        "selection": "validation PR-AUC over the grid; iterations by early stopping on validation",
        "calibration": {"method": "isotonic", "input": "raw_score", "fitted_on": "valid",
                        "rows": int(len(va)), "knots": int(len(xs))},
        "risk_score": "floor(calibrated probability * 100), clamped to 0-99; not a percentile",
        "feature_stats": stats,
        "data": {
            "source": args.source, "synthetic": args.source == "synthetic",
            "export_csv": csv.name, "sha256": rg.fingerprint(csv),
            "rows": {"train": int(len(tr)), "valid": int(len(va))},
            "fraud_rate": {"train": float(y_tr.mean()), "valid": float(y_va.mean())},
            "months": {"train": sorted(int(m) for m in tr["month"].unique()),
                       "valid": sorted(int(m) for m in va["month"].unique())},
        },
        "versions": {"lightgbm": lgb.__version__, "scikit-learn": sklearn.__version__,
                     "numpy": np.__version__, "pandas": pd.__version__},
        "trained_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
    }
    (args.out / "metadata.json").write_text(json.dumps(meta, indent=1))

    # Baselines for evaluate.py.
    base = args.out / "baselines"
    base.mkdir(exist_ok=True)
    lraw["_booster"].save_model(str(base / "lgbm_raw.txt"), num_iteration=lraw["best_iteration"])
    with open(base / "logreg.pkl", "wb") as f:
        pickle.dump(lr["_model"], f)

    args.results.mkdir(parents=True, exist_ok=True)
    validation = {
        "synthetic": args.source == "synthetic",
        "data": meta["data"],
        "chosen": {
            "logreg": public([lr])[0],
            "lgbm_raw": public([lraw])[0] | {"features": raw_feats},
            "lgbm_full": public([full])[0] | {"features": feats},
        },
        "rules": rules_baseline(args.rules_predictions, va),
        "runs": {"logreg": public(lr_runs), "lgbm_raw": public(lraw_runs), "lgbm_full": public(full_runs)},
    }
    (args.results / "validation.json").write_text(json.dumps(validation, indent=1))
    print(f"wrote {args.out} and {args.results / 'validation.json'}")


if __name__ == "__main__":
    main()
