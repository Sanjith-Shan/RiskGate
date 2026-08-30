"""Write the Go evaluator's parity fixtures to internal/model/testdata.

Each fixture is a small LightGBM model trained to exercise one part of the
text format (missing-value modes, categorical bitsets, single-leaf trees),
plus input rows and LightGBM's own raw scores for them. The Go tests assert
bit-identical scores on every row.

Inputs are not only training-like rows: every split threshold in the model,
the next double above and below it, zeros of both signs, values inside
LightGBM's zero threshold, NaN and infinities are fed in too, because that is
where an evaluator that is merely close gets a split wrong.

Floats are written with repr(), the shortest string that round-trips, so Go's
strconv.ParseFloat recovers the exact bits.

    python python/gen_fixtures.py
"""

import gzip
import json
import re
from pathlib import Path

import lightgbm as lgb
import numpy as np
from sklearn.isotonic import IsotonicRegression

from riskgate import RAW_FIELDS, interpolate

OUT = Path(__file__).resolve().parent.parent / "internal" / "model" / "testdata"
BASE = {"objective": "binary", "verbose": -1, "seed": 7, "deterministic": True,
        "force_row_wise": True, "num_threads": 1}


def thresholds(booster):
    """Numerical split thresholds per feature, from the model dump."""
    out = {}

    def walk(n):
        if "split_index" not in n:
            return
        if n["decision_type"] == "<=":
            out.setdefault(n["split_feature"], set()).add(float(n["threshold"]))
        walk(n["left_child"])
        walk(n["right_child"])

    for t in booster.dump_model()["tree_info"]:
        walk(t["tree_structure"])
    return out


def probe_rows(booster, X, n_random, rng, cat_cols=(), n_cats=None):
    """Training-like rows, then rows with one column forced to an edge value."""
    rows = [X[i].copy() for i in rng.choice(len(X), n_random, replace=False)]
    edges = [0.0, -0.0, 1e-40, -1e-40, 1e-35, -1e-35, 1.0000000180025095e-35,
             np.nan, np.inf, -np.inf]
    for f, ts in thresholds(booster).items():
        for t in ts:
            for v in (t, np.nextafter(t, np.inf), np.nextafter(t, -np.inf)):
                r = X[rng.integers(len(X))].copy()
                r[f] = v
                rows.append(r)
    for f in range(X.shape[1]):
        vals = list(edges)
        if f in cat_cols:
            k = n_cats[f]
            vals += [-1.0, -0.5, -3.0, 0.999, 3.7, k - 1, k, k + 1, 31, 32, 33, 63, 64,
                     65, 1000, 2**31, 1e10, -1e10]
        for v in vals:
            for _ in range(3):
                r = X[rng.integers(len(X))].copy()
                r[f] = v
                rows.append(r)
    return np.array(rows)


def decision_types(text):
    out = set()
    for line in text.splitlines():
        if line.startswith("decision_type="):
            out.update(int(v) for v in line.split("=", 1)[1].split())
    return out


def write_fixture(name, model_text, X, compress=False):
    d = OUT / name
    d.mkdir(parents=True, exist_ok=True)
    for old in d.glob("model.txt*"):
        old.unlink()
    if compress:  # the benchmark model; gzip keeps the repo small
        # mtime=0 keeps the file byte-identical across runs.
        with open(d / "model.txt.gz", "wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", mtime=0) as f:
            f.write(model_text.encode())
    else:
        (d / "model.txt").write_text(model_text)
    # Score with a booster loaded from the text, the same bytes Go reads.
    raw = lgb.Booster(model_str=model_text).predict(X, raw_score=True)
    lines = [",".join(f"f{i}" for i in range(X.shape[1])) + ",expected_raw\n"]
    for row, y in zip(X, raw):
        lines.append(",".join(repr(float(v)) for v in row) + "," + repr(float(y)) + "\n")
    for old in d.glob("cases.csv*"):
        old.unlink()
    if compress:
        with open(d / "cases.csv.gz", "wb") as raw_f, gzip.GzipFile(fileobj=raw_f, mode="wb", mtime=0) as f:
            f.write("".join(lines).encode())
    else:
        (d / "cases.csv").write_text("".join(lines))
    print(f"{name}: {len(X)} rows, {model_text.count('Tree=')} trees")


def fixture_nan_missing(rng):
    n = 6000
    X = rng.normal(size=(n, 4))
    X[rng.random((n, 4)) < 0.25] = np.nan
    # Missingness predicts fraud in feature 0 and legitimacy in feature 1, so
    # LightGBM learns default-left for some splits and default-right for others.
    logit = 0.8 * np.nan_to_num(X[:, 2]) + 1.5 * np.isnan(X[:, 0]) - 1.5 * np.isnan(X[:, 1])
    y = (logit + rng.logistic(size=n) > 0).astype(int)
    b = lgb.train({**BASE, "num_leaves": 15, "min_data_in_leaf": 20}, lgb.Dataset(X, y), 40)
    text = b.model_to_string()
    dts = decision_types(text)
    assert {8, 10} <= dts, f"want NaN-missing splits both ways, got {dts}"
    write_fixture("nan_missing", text, probe_rows(b, X, 400, rng))


def fixture_zero_missing(rng):
    n = 6000
    X = rng.normal(size=(n, 4))
    X[rng.random((n, 4)) < 0.25] = 0.0
    X[rng.random((n, 4)) < 0.05] = np.nan
    logit = 0.8 * X[:, 2] + 1.5 * (X[:, 0] == 0) - 1.5 * (X[:, 1] == 0)
    y = (np.nan_to_num(logit) + rng.logistic(size=n) > 0).astype(int)
    b = lgb.train({**BASE, "num_leaves": 15, "zero_as_missing": True}, lgb.Dataset(X, y), 40)
    text = b.model_to_string()
    dts = decision_types(text)
    assert {4, 6} <= dts, f"want zero-missing splits both ways, got {dts}"
    write_fixture("zero_missing", text, probe_rows(b, X, 400, rng))


def fixture_no_missing(rng):
    n = 4000
    X = rng.normal(size=(n, 3))
    y = (X[:, 0] - X[:, 1] + rng.logistic(size=n) > 0).astype(int)
    b = lgb.train({**BASE, "num_leaves": 7, "use_missing": False}, lgb.Dataset(X, y), 20)
    text = b.model_to_string()
    assert decision_types(text) <= {0, 2}, decision_types(text)
    # NaN at prediction time must be treated as 0 by a missing-type-None split.
    write_fixture("no_missing", text, probe_rows(b, X, 200, rng))


def fixture_categorical(rng):
    n = 20000
    k_big, k_small, k_tiny = 120, 10, 3
    X = np.column_stack([
        rng.integers(0, k_big, n).astype(float),
        rng.integers(0, k_small, n).astype(float),
        rng.normal(size=n),
        rng.integers(0, k_tiny, n).astype(float),
    ])
    X[rng.random(n) < 0.1, 0] = np.nan
    X[rng.random(n) < 0.1, 1] = np.nan
    risky = rng.permutation(k_big)[:40]  # spread over all four bitset words
    logit = (1.6 * np.isin(X[:, 0], risky) + 0.7 * (X[:, 1] % 3 == 0)
             + 0.5 * X[:, 2] + 0.6 * (X[:, 3] == 1) - 1.0)
    y = (logit + rng.logistic(size=n) > 0).astype(int)
    params = {**BASE, "num_leaves": 31, "max_cat_to_onehot": 4, "max_cat_threshold": 64,
              "min_data_per_group": 10, "cat_smooth": 1, "cat_l2": 1}
    b = lgb.train(params, lgb.Dataset(X, y, categorical_feature=[0, 1, 3]), 60)
    text = b.model_to_string()
    words = [len(line.split("=")[1].split()) for line in text.splitlines() if line.startswith("cat_threshold=")]
    bounds = [list(map(int, line.split("=")[1].split())) for line in text.splitlines()
              if line.startswith("cat_boundaries=")]
    widest = max(bd[i + 1] - bd[i] for bd in bounds for i in range(len(bd) - 1))
    assert widest >= 3, f"want multi-word bitsets, widest is {widest} words"
    assert sum(words) > 0
    write_fixture("categorical", text,
                  probe_rows(b, X, 600, rng, cat_cols=(0, 1, 3), n_cats={0: k_big, 1: k_small, 3: k_tiny}))


def fixture_single_leaf(rng):
    # A model that could not split at all: one constant tree.
    X = rng.normal(size=(300, 2))
    y = (rng.random(300) < 0.3).astype(int)
    b = lgb.train({**BASE, "min_gain_to_split": 1e9}, lgb.Dataset(X, y), 5)
    text = b.model_to_string()
    assert "num_leaves=1\n" in text
    write_fixture("single_leaf", text, probe_rows(b, X, 50, rng))

    # Single-leaf trees between ordinary ones. LightGBM stops training at the
    # first one, so splice them into a trained model's text; LightGBM itself
    # then loads and scores the spliced model, so parity is still against it.
    X = rng.normal(size=(3000, 3))
    y = (X[:, 0] + rng.logistic(size=3000) > 0).astype(int)
    b = lgb.train({**BASE, "num_leaves": 7}, lgb.Dataset(X, y), 6)
    text = b.model_to_string()
    head, rest = text.split("\nTree=0\n", 1)
    trees_text, tail = rest.split("\nend of trees\n", 1)
    trees = ("Tree=0\n" + trees_text).split("\n\n\nTree=")
    trees = [t if i == 0 else "Tree=" + t for i, t in enumerate(trees)]
    leaf = ("num_leaves=1\nnum_cat=0\nsplit_feature=\nsplit_gain=\nthreshold=\ndecision_type=\n"
            "left_child=\nright_child=\nleaf_value={v}\nleaf_weight=\nleaf_count=0\n"
            "internal_value=\ninternal_weight=\ninternal_count=\nis_linear=0\nshrinkage=0.1\n")
    mixed = []
    for i, t in enumerate(trees):
        mixed.append(re.sub(r"^Tree=\d+\n", "", t.strip("\n")) + "\n")
        if i % 2 == 0:
            mixed.append(leaf.format(v=repr(float(rng.normal() * 0.05))))
    head = re.sub(r"\ntree_sizes=[^\n]*", "", head)  # sizes no longer hold; LightGBM parses without them
    body = "".join(f"Tree={i}\n{t}\n\n" for i, t in enumerate(mixed))
    spliced = head + "\n" + body + "end of trees\n" + tail
    write_fixture("single_leaf_mixed", spliced, probe_rows(lgb.Booster(model_str=spliced), X, 100, rng))


def catalog_features():
    """Model inputs in the order of schema.Default(), without risk_score."""
    velocity = []
    for e in ["card", "uid", "device", "email"]:
        for w in ["1h", "24h", "7d"]:
            velocity += [f"{e}_txn_count_{w}", f"{e}_amount_sum_{w}"]
        velocity += [f"{e}_mean_amount_7d", f"{e}_amount_ratio_7d",
                     f"{e}_seconds_since_first", f"{e}_seconds_since_last"]
    velocity += ["distinct_cards_per_device_24h", "distinct_cards_per_email_24h"]
    return RAW_FIELDS + velocity


def fixture_large(rng):
    """The realistic model the benchmarks use: 500 trees, 63 leaves, and the
    53 catalog features, the 7 string fields as categoricals."""
    names = catalog_features()
    cats = {names.index(k): n for k, n in [
        ("product_code", 5), ("card_network", 4), ("card_type", 4), ("purchaser_email_domain", 60),
        ("recipient_email_domain", 60), ("device_type", 2), ("device_info", 90)]}
    n, p = 40000, len(names)
    X = np.abs(rng.normal(size=(n, p))) * rng.integers(0, 20, (n, p))
    for j, k in cats.items():
        X[:, j] = rng.integers(0, k, n)
    X[rng.random((n, p)) < 0.15] = np.nan
    w = rng.normal(size=p) * 0.1
    email = names.index("purchaser_email_domain")
    logit = np.nan_to_num(X) @ w * 0.3 + (np.nan_to_num(X[:, email]) % 7 == 0) - 3
    y = (logit + rng.logistic(size=n) > 0).astype(int)
    params = {**BASE, "num_leaves": 63, "learning_rate": 0.03, "min_data_in_leaf": 20,
              "feature_fraction": 0.8, "bagging_fraction": 0.8, "bagging_freq": 1,
              "max_cat_to_onehot": 4, "min_data_per_group": 20, "cat_smooth": 5}
    ds = lgb.Dataset(X, y, feature_name=names, categorical_feature=list(cats))
    b = lgb.train(params, ds, 500)
    assert b.num_trees() == 500
    X_probe = np.vstack([X[rng.choice(n, 1500, replace=False)],
                         probe_rows(b, X, 0, rng, cat_cols=tuple(cats), n_cats=cats)[:1500]])
    write_fixture("large", b.model_to_string(), X_probe, compress=True)


def fixture_calibration(rng):
    raw = rng.normal(-3, 1.5, 5000)
    y = (rng.random(5000) < 1 / (1 + np.exp(-(raw + 0.5)))).astype(int)
    iso = IsotonicRegression(out_of_bounds="clip", y_min=0.0, y_max=1.0).fit(raw, y)
    xs, ys = iso.X_thresholds_.astype(float), iso.y_thresholds_.astype(float)
    d = OUT / "calibration"
    d.mkdir(parents=True, exist_ok=True)
    (d / "calibration.json").write_text(json.dumps(
        {"method": "isotonic", "input": "raw_score", "x": xs.tolist(), "y": ys.tolist()}, indent=1))
    probe = np.concatenate([
        xs, (xs[:-1] + xs[1:]) / 2, np.nextafter(xs, np.inf), np.nextafter(xs, -np.inf),
        rng.normal(-3, 2, 2000), [-1e300, 1e300, -np.inf, np.inf, np.nan],
    ])
    expected = interpolate(probe, xs, ys)
    finite = np.isfinite(probe)
    assert np.max(np.abs(expected - np.interp(probe, xs, ys))[finite]) < 1e-15
    assert np.max(np.abs(expected[finite] - iso.predict(probe[finite]))) < 1e-12
    with open(d / "cases.csv", "w") as f:
        f.write("raw,expected_prob\n")
        for x, p in zip(probe, expected):
            f.write(f"{float(x)!r},{float(p)!r}\n")
    print(f"calibration: {len(xs)} knots, {len(probe)} rows")


def main():
    rng = np.random.default_rng(20260923)
    fixture_nan_missing(rng)
    fixture_zero_missing(rng)
    fixture_no_missing(rng)
    fixture_categorical(rng)
    fixture_single_leaf(rng)
    fixture_large(rng)
    fixture_calibration(rng)


if __name__ == "__main__":
    main()
