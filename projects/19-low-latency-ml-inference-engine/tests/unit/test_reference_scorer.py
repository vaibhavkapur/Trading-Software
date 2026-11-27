"""Unit tests for the pure-Python deterministic reference scorer."""

from __future__ import annotations

import unittest
from pathlib import Path

from inference_engine.artifact import load_verified_artifact
from inference_engine.contracts import InferenceRequest
from inference_engine.reference_scorer import generate_synthetic_lob_dataset
from inference_engine.runtime_adapter import PythonReferenceAdapter


PROJECT_ROOT = Path(__file__).resolve().parents[2]
V1_BUNDLE = PROJECT_ROOT / "data" / "fixtures" / "models" / "lob_alpha_mlp" / "v1.0.0"


class TestReferenceScorer(unittest.TestCase):
    def test_synthetic_dataset_is_deterministic_and_chronological(self) -> None:
        a = generate_synthetic_lob_dataset(num_samples=50, seed=42)
        b = generate_synthetic_lob_dataset(num_samples=50, seed=42)
        self.assertEqual(a, b)
        timestamps = [r["event_ts_utc"] for r in a]
        self.assertEqual(timestamps, sorted(timestamps))

    def test_python_reference_matches_golden_vectors(self) -> None:
        art = load_verified_artifact(V1_BUNDLE, verify_sha256=True)
        adapter = PythonReferenceAdapter(art)
        res = adapter.verify_parity()
        self.assertTrue(res["passed"])
        self.assertLessEqual(res["max_abs_error"], 1e-12)

    def test_python_reference_request_observability(self) -> None:
        art = load_verified_artifact(V1_BUNDLE, verify_sha256=True)
        adapter = PythonReferenceAdapter(art)
        ts = "2026-01-15T15:00:00.000Z"
        req = InferenceRequest(
            request_id="unit-ref-001",
            instrument_id="SYN-EQUITY-A",
            currency="USD",
            event_ts_utc=ts,
            available_ts_utc=ts,
            inference_ts_utc=ts,
            features=art.golden_vectors[0].raw_features,
        )
        resp = adapter.score_request(req)
        self.assertEqual(resp.model_id, "lob_alpha_mlp")
        self.assertEqual(resp.model_version, "1.0.0")
        self.assertEqual(resp.runtime_name, "python_reference")
        self.assertFalse(resp.order_execution_enabled)
        self.assertAlmostEqual(
            resp.prediction, art.golden_vectors[0].expected_prediction, places=12
        )


if __name__ == "__main__":
    unittest.main()
