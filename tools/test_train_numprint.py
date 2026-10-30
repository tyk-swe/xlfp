"""Tests for tools/train_numprint.py (offline; numpy-optional)."""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))

import train_numprint

try:
    import numpy  # noqa: F401

    HAS_NUMPY = True
except ImportError:
    HAS_NUMPY = False


def sample_rows():
    rows = []
    # Two well-separated classes in 4 dims.
    for i in range(6):
        rows.append({"entry_id": "a", "sample_id": f"a{i}", "feature": [1.0 + i * 0.01, 0.0, 0.2, 0.0]})
        rows.append({"entry_id": "b", "sample_id": f"b{i}", "feature": [0.0, 1.0 + i * 0.01, 0.0, 0.3]})
    return rows


class TestLoadFeatures(unittest.TestCase):
    def test_load_and_validate(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "features.json")
            with open(p, "w") as f:
                json.dump({"version": "t", "dims": 4, "rows": sample_rows()}, f)
            dims, rows = train_numprint.load_features(p)
            self.assertEqual(dims, 4)
            self.assertEqual(len(rows), 12)

    def test_rejects_ragged_rows(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "features.json")
            rows = sample_rows()
            rows[0]["feature"] = [1.0]
            with open(p, "w") as f:
                json.dump({"dims": 4, "rows": rows}, f)
            with self.assertRaises(ValueError):
                train_numprint.load_features(p)


@unittest.skipUnless(HAS_NUMPY, "numpy not installed")
class TestFitLDA(unittest.TestCase):
    def test_projection_separates_classes(self):
        rows = sample_rows()
        params = train_numprint.fit_lda(4, rows)
        self.assertEqual(len(params["projection"]), 4)
        self.assertEqual(set(params["means"]), {"a", "b"})
        # Nearest projected mean must classify every row correctly:
        # that is what the Go ranker's LDA scorer relies on.
        W = params["projection"]
        for row in rows:
            proj = [sum(row["feature"][i] * W[i][j] for i in range(4)) for j in range(len(W[0]))]
            best = min(params["means"], key=lambda e: sum((p - m) ** 2 for p, m in zip(proj, params["means"][e])))
            self.assertEqual(best, row["entry_id"], row["sample_id"])


if __name__ == "__main__":
    unittest.main()
