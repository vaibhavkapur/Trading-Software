"""Integration tests verifying numerical parity between Python reference and C++20 native runtime."""

from __future__ import annotations

import json
import unittest
from pathlib import Path

from inference_engine.artifact import load_verified_artifact
from inference_engine.contracts import BatchInferenceRequest, InferenceRequest
from inference_engine.runtime_adapter import CppNativeAdapter, PythonReferenceAdapter


PROJECT_ROOT = Path(__file__).resolve().parents[2]
MODELS_ROOT = PROJECT_ROOT / "data" / "fixtures" / "models" / "lob_alpha_mlp"
FIXTURE_JSONL = PROJECT_ROOT / "data" / "fixtures" / "synthetic_lob_features.jsonl"


class TestNativeParity(unittest.TestCase):
    def test_single_and_batch_parity_across_v1_and_v2(self) -> None:
        records = [
            json.loads(line)
            for line in FIXTURE_JSONL.read_text(encoding="utf-8").splitlines()[:100]
        ]
        ts = "2026-01-15T15:00:00.000Z"

        for version in ("1.0.0", "2.0.0"):
            art = load_verified_artifact(MODELS_ROOT / f"v{version}", verify_sha256=True)
            py = PythonReferenceAdapter(art)
            cpp = CppNativeAdapter(art)
            try:
                tol = art.manifest.output.prediction_tolerance_abs
                self.assertTrue(cpp.verify_parity()["passed"])

                reqs = [
                    InferenceRequest(
                        request_id=f"par-{version}-{i}",
                        instrument_id=rec["instrument_id"],
                        currency=rec["currency"],
                        event_ts_utc=ts,
                        available_ts_utc=ts,
                        inference_ts_utc=ts,
                        features=rec["features"],
                    )
                    for i, rec in enumerate(records)
                ]

                # Single parity
                max_err = 0.0
                for req in reqs[:30]:
                    r_py = py.score_request(req)
                    r_cpp = cpp.score_request(req)
                    err = abs(r_py.prediction - r_cpp.prediction)
                    max_err = max(max_err, err)
                    self.assertLessEqual(err, tol)
                self.assertLessEqual(max_err, tol)

                # Batch parity
                batch_req = BatchInferenceRequest(batch_id=f"batch-{version}", requests=reqs)
                b_py = py.score_batch(batch_req)
                b_cpp = cpp.score_batch(batch_req)
                self.assertEqual(len(b_py.responses), len(b_cpp.responses))
                for rp, rc in zip(b_py.responses, b_cpp.responses):
                    self.assertLessEqual(abs(rp.prediction - rc.prediction), tol)
            finally:
                cpp.close()


if __name__ == "__main__":
    unittest.main()
