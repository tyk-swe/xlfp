"""Offline LDA trainer for the number fingerprint.

Reads the labeled feature vectors written by
`xlfp retrain --export-features features.json`, fits a linear
discriminant projection, and writes the parameter JSON consumed by
`xlfp retrain --lda lda.json`. Implemented from the standard LDA
definition (between-class vs within-class scatter, ridge-regularized);
see docs/clean-room.md.

Requires numpy (the project .venv: pip install -r tools/requirements.txt).
"""

import argparse
import json
import sys


def load_features(path):
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    rows = doc.get("rows") or []
    if not rows:
        raise ValueError("features file has no rows")
    dims = doc.get("dims") or len(rows[0]["feature"])
    for row in rows:
        if len(row["feature"]) != dims:
            raise ValueError(f"row {row.get('sample_id')} has {len(row['feature'])} dims, want {dims}")
    return dims, rows


def fit_lda(dims, rows, max_axes=16, ridge=1e-3):
    import numpy as np

    entries = sorted({r["entry_id"] for r in rows})
    if len(entries) < 2:
        raise ValueError("need at least 2 entries to fit LDA")
    X = np.array([r["feature"] for r in rows], dtype=np.float64)
    overall = X.mean(axis=0)
    means = {}
    sw = np.zeros((dims, dims))
    sb = np.zeros((dims, dims))
    for eid in entries:
        Xi = X[[i for i, r in enumerate(rows) if r["entry_id"] == eid]]
        m = Xi.mean(axis=0)
        means[eid] = m
        centered = Xi - m
        sw += centered.T @ centered
        d = (m - overall).reshape(-1, 1)
        sb += len(Xi) * (d @ d.T)
    # A positive floor also covers deterministic samples with zero scatter.
    scale = np.trace(sw) / dims if dims else 1.0
    sw += max(ridge * scale, 1e-12) * np.eye(dims)
    # Solve the generalized eigenproblem Sb v = lambda Sw v via
    # whitening with the Cholesky factor of Sw.
    L = np.linalg.cholesky(sw)
    Linv = np.linalg.inv(L)
    M = Linv @ sb @ Linv.T
    M = (M + M.T) / 2.0
    evals, evecs = np.linalg.eigh(M)
    order = np.argsort(evals)[::-1]
    axes = min(max_axes, len(entries) - 1, dims)
    W = Linv.T @ evecs[:, order[:axes]]  # dims x axes
    # Normalize columns for a stable distance scale.
    for j in range(axes):
        n = np.linalg.norm(W[:, j])
        if n > 0:
            W[:, j] /= n
    return {
        "projection": W.tolist(),
        "means": {eid: (means[eid] @ W).tolist() for eid in entries},
    }


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--features", required=True, help="feature export JSON from xlfp retrain --export-features")
    ap.add_argument("--out", required=True, help="LDA params JSON to write")
    ap.add_argument("--max-axes", type=int, default=16)
    args = ap.parse_args(argv)
    try:
        import numpy  # noqa: F401
    except ImportError:
        print("numpy is required: use the project .venv (pip install -r tools/requirements.txt)", file=sys.stderr)
        return 2
    dims, rows = load_features(args.features)
    params = fit_lda(dims, rows, max_axes=args.max_axes)
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(params, f, indent=2)
        f.write("\n")
    print(f"wrote {args.out}: {len(rows)} rows, {dims} dims, {len(params['means'])} entries")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
