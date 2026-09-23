"""Score the test month, once, and publish whatever it says.

The test month is touched exactly once. Before any test label is read this
script writes results/test_touched.lock (time, git commit, data fingerprint).
If the lock already exists it refuses to run, unless it is given
--i-know-this-is-a-second-look, which it records in the lock and stamps on
every output. Nothing here is tuned: thresholds come from validation.json,
written by train.py.

Metrics, per model, on the test month:
  roc_auc, pr_auc     PR-AUC is average precision.
  recall_at_1pct_fpr  Fraud recall at the point of the test ROC curve with the
                      highest recall whose false-positive rate is <= 1%. A
                      reading of the curve, not a threshold anyone could pick.
  ..._valid_threshold The deployable version: the threshold that gave 1% FPR
                      on validation, applied to test, with the realized FPR.
  fraud_dollar_recall_at_1pct_legit_dollars
                      Share of fraud dollars blocked when payments are blocked
                      from the riskiest down (whole score ties at a time) until
                      the legitimate dollars blocked would exceed 1% of all
                      legitimate dollars in the test month. Also reported at
                      the validation-chosen threshold, with realized spend.
  brier, ece          For the served model only, before and after isotonic
                      calibration. ECE = sum over bins of (share of rows) *
                      |mean predicted - fraud rate|, with 10 equal-width bins
                      and, because most payments sit near 0, with 20
                      equal-count bins too.

    python python/evaluate.py --export data/export --model-dir models/current --results results
"""

import argparse
import datetime
import json
import pickle
import sys
from pathlib import Path

import lightgbm as lgb
import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

import riskgate as rg  # noqa: E402

LOCK = "test_touched.lock"
SECOND_LOOK = "--i-know-this-is-a-second-look"


def git_commit(start):
    """HEAD's commit hash, read from .git without running git."""
    for d in [start, *start.parents]:
        head = d / ".git" / "HEAD"
        if not head.is_file():
            continue
        ref = head.read_text().strip()
        if not ref.startswith("ref: "):
            return ref
        name = ref[5:]
        loose = d / ".git" / name
        if loose.is_file():
            return loose.read_text().strip()
        packed = d / ".git" / "packed-refs"
        if packed.is_file():
            for line in packed.read_text().splitlines():
                if line.endswith(" " + name):
                    return line.split()[0]
        return "unknown (unborn branch)"
    return "unknown (not a git checkout)"


def take_lock(results, second_look, fingerprint):
    """Record this look at the test month, or refuse if it is not the first."""
    path = results / LOCK
    look = {
        "at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "git_commit": git_commit(Path(__file__).resolve().parent),
        "data_sha256": fingerprint,
        "argv": sys.argv[1:],
    }
    if path.exists():
        lock = json.loads(path.read_text())
        if not second_look:
            first = lock["looks"][0]
            raise SystemExit(
                f"refusing: the test month was already evaluated at {first['at']} "
                f"(commit {first['git_commit']}); see {path}.\n"
                f"Publish that result. To look again anyway, pass {SECOND_LOOK}; it is recorded.")
        look["second_look"] = True
        lock["looks"].append(look)
    else:
        lock = {"note": "The test month has been evaluated. Every look is listed.", "looks": [look]}
    results.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(lock, indent=1))
    return len(lock["looks"])


def ranking_metrics(y, s, amount, chosen):
    rec_valid, fpr_valid = rg.rate_at_threshold(y, s, chosen["threshold_1pct_fpr"])
    usd_valid, share_valid = rg.dollars_at_threshold(y, s, amount, chosen["threshold_1pct_legit_dollars"])
    _, rec, fpr = rg.threshold_at_fpr(y, s, 0.01)
    _, usd, share = rg.threshold_at_dollar_budget(y, s, amount, 0.01)
    return {
        "roc_auc": rg.roc_auc(y, s),
        "pr_auc": rg.pr_auc(y, s),
        "recall_at_1pct_fpr": rec,
        "recall_at_valid_threshold": rec_valid, "fpr_at_valid_threshold": fpr_valid,
        "fraud_dollar_recall_at_1pct_legit_dollars": usd,
        "fraud_dollar_recall_at_valid_threshold": usd_valid,
        "legit_dollar_share_at_valid_threshold": share_valid,
    }


def calibration_metrics(y, p):
    return {"brier": rg.brier(y, p), "ece_10_equal_width": rg.ece(y, p, 10),
            "ece_20_equal_count": rg.ece(y, p, 20, equal_mass=True)}


def reliability_plot(y, curves, path, title):
    """Observed fraud rate against mean predicted probability, 20 equal-count
    bins, log-log so the low-probability bins where most payments sit are
    readable."""
    colors = {"uncalibrated": "#eb6834", "isotonic": "#2a78d6"}  # categorical slots 2 and 1
    fig, ax = plt.subplots(figsize=(6, 5.2), dpi=150)
    points = {}
    for name, p in curves.items():
        bins = np.array_split(np.argsort(p, kind="stable"), 20)
        mp = np.array([p[b].mean() for b in bins])
        fr = np.array([y[b].mean() for b in bins])
        keep = (mp > 0) & (fr > 0)  # an empty-fraud bin has no place on a log axis
        points[name] = mp[keep], fr[keep]
    smallest = min(v.min() for xy in points.values() for v in xy if len(v))
    lo = 10 ** np.floor(np.log10(smallest))
    ax.plot([lo, 1], [lo, 1], color="#9a9a93", lw=1, ls="--", zorder=1, label="perfect calibration")
    for name, (mp, fr) in points.items():
        ax.plot(mp, fr, color=colors[name], lw=2, marker="o", ms=5,
                markeredgecolor="white", markeredgewidth=1, label=name, zorder=3)
    ax.set(xscale="log", yscale="log", xlim=(lo, 1), ylim=(lo, 1),
           xlabel="mean predicted fraud probability (bin)", ylabel="observed fraud rate (bin)")
    ax.set_title(title, fontsize=10, loc="left")
    ax.grid(True, which="major", color="#e6e5df", lw=0.6)
    for side in ("top", "right"):
        ax.spines[side].set_visible(False)
    ax.legend(frameon=False, loc="upper left", fontsize=9)
    fig.tight_layout()
    fig.savefig(path)
    plt.close(fig)


def markdown(out):
    tag = "SYNTHETIC DATA. " if out["synthetic"] else ""
    lines = [f"# Test month results{' (SECOND LOOK)' if out['look'] > 1 else ''}", "",
             f"{tag}Test month, touched once. {out['rows']} payments, {out['fraud_rate']:.2%} fraud. "
             "Thresholds marked 'valid thr.' were chosen on the validation month.", "",
             "| model | ROC-AUC | PR-AUC | recall @ 1% FPR | recall @ valid thr. (FPR) | "
             "fraud $ recall @ 1% legit $ | fraud $ recall @ valid thr. (legit $) |",
             "|---|---|---|---|---|---|---|"]
    for name, m in out["models"].items():
        if "skipped" in m:
            lines.append(f"| {name} | skipped: {m['skipped']} | | | | | |")
        elif "roc_auc" not in m:
            lines.append(f"| {name} (one operating point) | | | | {m['recall']:.1%} ({m['fpr']:.2%}) | | "
                         f"{m['fraud_dollar_recall']:.1%} ({m['legit_dollar_share_blocked']:.2%}) |")
        else:
            lines.append(
                f"| {name} | {m['roc_auc']:.4f} | {m['pr_auc']:.4f} | {m['recall_at_1pct_fpr']:.1%} | "
                f"{m['recall_at_valid_threshold']:.1%} ({m['fpr_at_valid_threshold']:.2%}) | "
                f"{m['fraud_dollar_recall_at_1pct_legit_dollars']:.1%} | "
                f"{m['fraud_dollar_recall_at_valid_threshold']:.1%} ({m['legit_dollar_share_at_valid_threshold']:.2%}) |")
    c = out["calibration"]
    lines += ["", "Calibration of lgbm_full (the served model):", "",
              "| | Brier | ECE, 10 equal-width bins | ECE, 20 equal-count bins |", "|---|---|---|---|"]
    for k in ("uncalibrated", "isotonic"):
        lines.append(f"| {k} | {c[k]['brier']:.5f} | {c[k]['ece_10_equal_width']:.5f} | "
                     f"{c[k]['ece_20_equal_count']:.5f} |")
    lines += ["", "Definitions are in python/evaluate.py. ![reliability](reliability.png)", ""]
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--export", required=True, type=Path)
    ap.add_argument("--model-dir", required=True, type=Path)
    ap.add_argument("--results", default=Path("results"), type=Path)
    ap.add_argument("--rules-predictions", type=Path, help="Go rule engine output: TransactionID,flagged")
    ap.add_argument(SECOND_LOOK, dest="second_look", action="store_true")
    args = ap.parse_args()

    csv = args.export / "export.csv"
    meta = json.loads((args.model_dir / "metadata.json").read_text())
    validation = json.loads((args.results / "validation.json").read_text())
    fp = rg.fingerprint(csv)
    if fp != meta["data"]["sha256"]:
        raise SystemExit(f"{csv} is not the export the model was trained on (sha256 differs)")

    look = take_lock(args.results, args.second_look, fp)  # before any test label is read

    feats = meta["feature_names"]
    te = rg.read_export(csv, feats, splits=["test"])
    y, amount = te["isFraud"].to_numpy(), te["amount"].to_numpy()
    chosen = validation["chosen"]

    models = {}
    if args.rules_predictions:
        flags = te[["TransactionID"]].merge(pd.read_csv(args.rules_predictions), on="TransactionID", how="left")
        if flags["flagged"].isna().any():
            raise SystemExit("rules predictions do not cover every test row")
        f = flags["flagged"].to_numpy()
        rec, fpr = rg.rate_at_threshold(y, f, 1)
        usd, share = rg.dollars_at_threshold(y, f, amount, 1)
        models["rules"] = {"recall": rec, "fpr": fpr, "fraud_dollar_recall": usd,
                           "legit_dollar_share_blocked": share}
    else:
        models["rules"] = {"skipped": "no Go rule engine predictions given"}

    with open(args.model_dir / "baselines" / "logreg.pkl", "rb") as fh:
        logreg = pickle.load(fh)
    s = logreg.decision_function(te[feats])
    models["logreg"] = ranking_metrics(y, s, amount, chosen["logreg"]["valid"])

    raw_feats = chosen["lgbm_raw"]["features"]
    b_raw = lgb.Booster(model_file=str(args.model_dir / "baselines" / "lgbm_raw.txt"))
    s = b_raw.predict(te[raw_feats].to_numpy(dtype=np.float64), raw_score=True)
    models["lgbm_raw"] = ranking_metrics(y, s, amount, chosen["lgbm_raw"]["valid"])

    b = lgb.Booster(model_file=str(args.model_dir / "model.txt"))
    raw = b.predict(te[feats].to_numpy(dtype=np.float64), raw_score=True)
    models["lgbm_full"] = ranking_metrics(y, raw, amount, chosen["lgbm_full"]["valid"])

    xs, ys = rg.load_calibration(args.model_dir / "calibration.json")
    p_uncal, p_cal = rg.sigmoid(raw), rg.interpolate(raw, xs, ys)
    out = {
        "synthetic": bool(meta["data"]["synthetic"]),
        "look": look,
        "rows": int(len(te)), "fraud_rate": float(y.mean()),
        "data_sha256": fp,
        "models": models,
        "calibration": {"uncalibrated": calibration_metrics(y, p_uncal),
                        "isotonic": calibration_metrics(y, p_cal)},
        "risk_score_histogram": np.bincount(rg.risk_score(p_cal), minlength=100).tolist(),
    }
    (args.results / "test_metrics.json").write_text(json.dumps(out, indent=1))
    (args.results / "test_metrics.md").write_text(markdown(out))
    title = ("SYNTHETIC DATA: " if out["synthetic"] else "") + "Reliability of lgbm_full, test month"
    reliability_plot(y, {"uncalibrated": p_uncal, "isotonic": p_cal}, args.results / "reliability.png", title)
    print(markdown(out))


if __name__ == "__main__":
    main()
