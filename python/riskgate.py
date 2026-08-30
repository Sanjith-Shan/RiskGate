"""Shared helpers for RiskGate's offline scripts: reading the Go export and
applying the calibration exactly as the Go service does.

Python never computes a feature. Every model input comes from the CSV that
RiskGate's Go export (cmd/export) writes, using the same feature code the
service runs.
"""

import hashlib
import json
from pathlib import Path

import numpy as np
import pandas as pd

# Mirrors schema.RawFields in internal/schema/catalog.go: the columns taken
# from the transaction itself. Everything else in features.json is a velocity
# feature computed by RiskGate. train.py checks these names against the export.
RAW_FIELDS = [
    "amount", "product_code", "card_network", "card_type", "purchaser_email_domain",
    "recipient_email_domain", "device_type", "device_info", "distance",
    "billing_region", "billing_country_code",
]

META_COLUMNS = ["TransactionID", "TransactionDT", "split", "month", "isFraud", "amount"]


def load_features(export_dir):
    """features.json from the Go export: model input order and categoricals."""
    spec = json.loads((Path(export_dir) / "features.json").read_text())
    return spec["features"], spec.get("categorical", [])


def read_export(csv_path, features, splits=None, labels=True):
    """Read the Go export CSV.

    Floats are parsed with round_trip precision so every value has exactly
    the bits the Go export wrote; pandas' default parser can be off by an ulp,
    which would break train/serve parity before the model is even involved.
    Empty cells are NaN, which is how the export writes missing values and
    how LightGBM reads them. With labels=False the isFraud column is never
    read, so scripts that must not see labels cannot.
    """
    header = pd.read_csv(csv_path, nrows=0).columns
    missing = [c for c in features if c not in header]
    if missing:
        raise SystemExit(f"{csv_path}: export lacks feature columns {missing}")
    wanted = [c for c in META_COLUMNS if labels or c != "isFraud"]
    usecols = list(dict.fromkeys(wanted + list(features)))
    dtypes = {c: "float64" for c in features}
    dtypes.update({"TransactionID": "int64", "split": "string"})
    df = pd.read_csv(csv_path, usecols=usecols, dtype=dtypes, float_precision="round_trip",
                     keep_default_na=False, na_values=[""])
    if splits is not None:
        df = df[df["split"].isin(splits)].reset_index(drop=True)
    return df


def fingerprint(path, chunk=1 << 20):
    """sha256 of a file, recorded wherever results depend on it."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while block := f.read(chunk):
            h.update(block)
    return h.hexdigest()


def interpolate(raw, xs, ys):
    """The calibration map, identical to Calibrator.Apply in internal/model.

    np.interp semantics (clamp outside the knots, linear between them) with
    each arithmetic step as its own NumPy operation, so each rounds once and
    no compiler can fuse them into a multiply-add. np.interp itself agrees
    to within an ulp but its last bit depends on how NumPy was compiled.
    """
    raw = np.asarray(raw, dtype=np.float64)
    xs = np.asarray(xs, dtype=np.float64)
    ys = np.asarray(ys, dtype=np.float64)
    if len(xs) == 1:
        return np.where(np.isnan(raw), raw, ys[0])
    j = np.clip(np.searchsorted(xs, raw, side="right") - 1, 0, len(xs) - 2)
    with np.errstate(invalid="ignore", over="ignore"):  # inf inputs, replaced below
        slope = np.divide(np.subtract(ys[j + 1], ys[j]), np.subtract(xs[j + 1], xs[j]))
        step = np.multiply(slope, np.subtract(raw, xs[j]))
        out = np.add(step, ys[j])
    out = np.where(raw <= xs[0], ys[0], out)
    out = np.where(raw >= xs[-1], ys[-1], out)
    return np.where(np.isnan(raw), raw, out)


def load_calibration(path):
    c = json.loads(Path(path).read_text())
    assert c["method"] == "isotonic" and c["input"] == "raw_score", c
    return np.array(c["x"], dtype=np.float64), np.array(c["y"], dtype=np.float64)


def risk_score(p):
    """floor(p * 100) clamped to 0..99, as model.RiskScore in Go."""
    s = np.floor(np.asarray(p) * 100)
    return np.clip(np.nan_to_num(s, nan=0), 0, 99).astype(int)


# Metrics. Scores are "higher is riskier"; y is 1 for fraud.

def roc_auc(y, s):
    from sklearn.metrics import roc_auc_score
    return float(roc_auc_score(y, s))


def pr_auc(y, s):
    """Average precision, the step-wise area under the precision-recall curve."""
    from sklearn.metrics import average_precision_score
    return float(average_precision_score(y, s))


def threshold_at_fpr(y, s, fpr=0.01):
    """Lowest score threshold (flag if s >= t) whose false-positive rate on
    (y, s) is at most fpr. Returns (threshold, recall, realized fpr)."""
    from sklearn.metrics import roc_curve
    f, t, thr = roc_curve(y, s, drop_intermediate=False)
    i = np.flatnonzero(f <= fpr)[-1]
    return float(thr[i]), float(t[i]), float(f[i])


def rate_at_threshold(y, s, threshold):
    """(recall, fpr) when flagging s >= threshold."""
    y = np.asarray(y).astype(bool)
    flag = np.asarray(s) >= threshold
    return float(flag[y].mean()), float(flag[~y].mean())


def threshold_at_dollar_budget(y, s, amount, budget=0.01):
    """Block payments from the riskiest down, whole score ties at a time, for
    as long as the legitimate dollars blocked stay within budget (a fraction
    of all legitimate dollars). Returns (threshold, fraud-dollar recall,
    realized legitimate-dollar share)."""
    y = np.asarray(y).astype(bool)
    s = np.asarray(s, dtype=np.float64)
    amount = np.asarray(amount, dtype=np.float64)
    order = np.argsort(-s, kind="stable")
    s, y, amount = s[order], y[order], amount[order]
    legit = np.cumsum(np.where(y, 0.0, amount))
    fraud = np.cumsum(np.where(y, amount, 0.0))
    ends = np.flatnonzero(np.r_[s[1:] != s[:-1], True])  # last index of each score tie group
    ok = ends[legit[ends] <= budget * legit[-1]]
    if len(ok) == 0:
        return float("inf"), 0.0, 0.0
    k = ok[-1]
    return float(s[k]), float(fraud[k] / fraud[-1]), float(legit[k] / legit[-1])


def dollars_at_threshold(y, s, amount, threshold):
    """(fraud-dollar recall, legitimate-dollar share blocked) at s >= threshold."""
    y = np.asarray(y).astype(bool)
    flag = np.asarray(s) >= threshold
    amount = np.asarray(amount, dtype=np.float64)
    return (float(amount[flag & y].sum() / amount[y].sum()),
            float(amount[flag & ~y].sum() / amount[~y].sum()))


def brier(y, p):
    return float(np.mean((np.asarray(p) - np.asarray(y)) ** 2))


def ece(y, p, bins=10, equal_mass=False):
    """Expected calibration error: sum over bins of (bin share of rows) *
    |mean predicted - observed fraud rate|. Equal-width bins over [0, 1] by
    default; equal_mass puts the same number of rows in each bin instead,
    which is more informative when most probabilities are near zero."""
    y = np.asarray(y, dtype=np.float64)
    p = np.asarray(p, dtype=np.float64)
    if equal_mass:
        idx = np.array_split(np.argsort(p, kind="stable"), bins)
    else:
        b = np.minimum((p * bins).astype(int), bins - 1)
        idx = [np.flatnonzero(b == i) for i in range(bins)]
    return float(sum(len(i) / len(p) * abs(p[i].mean() - y[i].mean()) for i in idx if len(i)))


def signed_log1p(a):
    """sign(a) * log1p(|a|): tames heavy-tailed dollar and count columns for
    logistic regression. Module-level so the fitted pipeline pickles."""
    return np.sign(a) * np.log1p(np.abs(a))


def sigmoid(raw):
    return 1.0 / (1.0 + np.exp(-np.asarray(raw, dtype=np.float64)))
