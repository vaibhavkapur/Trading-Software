"""Required scenario tests for all 8 failure & validation modes from plan.md Section 5.

Scenarios verified:
1. Python/native parity
2. Missing feature
3. NaN (and Inf)
4. Invalid artifact (tampered weights + untrusted pickle)
5. Model reload (hot reload, failed corrupted reload preserving active model, rollback)
6. Overload (bounded max_in_flight shedding)
7. Cold start (cold load + verify + first score vs warm steady-state)
8. Latency tail regression (injected slowdown caught by regression gate)
"""

from __future__ import annotations

import shutil
import tempfile
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from inference_engine.artifact import load_verified_artifact
from inference_engine.benchmark import (
    check_latency_tail_regression,
    compute_latency_stats,
    measure_cold_start,
)
from inference_engine.contracts import EngineError, ErrorCode, InferenceRequest
from inference_engine.router import InferenceRouter
from inference_engine.runtime_adapter import CppNativeAdapter, PythonReferenceAdapter


PROJECT_ROOT = Path(__file__).resolve().parents[2]
MODELS_ROOT = PROJECT_ROOT / "data" / "fixtures" / "models" / "lob_alpha_mlp"
V1_DIR = MODELS_ROOT / "v1.0.0"
V2_DIR = MODELS_ROOT / "v2.0.0"


class TestRequiredFailureScenarios(unittest.TestCase):
    def setUp(self) -> None:
        self.art_v1 = load_verified_artifact(V1_DIR)
        self.valid_features = dict(self.art_v1.golden_vectors[0].raw_features)
        self.ts = "2026-01-15T15:00:00.000Z"

    def _make_req(
        self,
        req_id: str,
        features: dict[str, float] | None = None,
        event_ts: str | None = None,
        avail_ts: str | None = None,
    ) -> InferenceRequest:
        return InferenceRequest(
            request_id=req_id,
            instrument_id="SYN-EQUITY-A",
            currency="USD",
            event_ts_utc=event_ts or self.ts,
            available_ts_utc=avail_ts or self.ts,
            inference_ts_utc=self.ts,
            features=features if features is not None else self.valid_features,
        )

    def test_scenario_1_python_native_parity(self) -> None:
        py = PythonReferenceAdapter(self.art_v1)
        cpp = CppNativeAdapter(self.art_v1)
        try:
            tol = self.art_v1.manifest.output.prediction_tolerance_abs
            for gv in self.art_v1.golden_vectors:
                req = self._make_req(f"sc1-{gv.case_id}", features=gv.raw_features)
                r_py = py.score_request(req)
                r_cpp = cpp.score_request(req)
                self.assertLessEqual(abs(r_py.prediction - r_cpp.prediction), tol)
        finally:
            cpp.close()

    def test_scenario_2_missing_feature(self) -> None:
        router = InferenceRouter(runtime="cpp_native")
        try:
            router.load_version(V1_DIR, activate_as_primary=True)
            incomplete = dict(self.valid_features)
            incomplete.pop("bid_ask_spread_bps")
            with self.assertRaises(EngineError) as ctx:
                router.predict(self._make_req("sc2-missing", features=incomplete))
            self.assertEqual(ctx.exception.code, ErrorCode.MISSING_FEATURE)
            self.assertEqual(
                router.metrics_snapshot()["errors_by_code"].get("MISSING_FEATURE"), 1
            )
        finally:
            router.close()

    def test_scenario_3_nan_feature(self) -> None:
        router = InferenceRouter(runtime="cpp_native")
        try:
            router.load_version(V1_DIR, activate_as_primary=True)
            nan_feats = dict(self.valid_features)
            nan_feats["order_book_imbalance_l1"] = float("nan")
            with self.assertRaises(EngineError) as ctx:
                router.predict(self._make_req("sc3-nan", features=nan_feats))
            self.assertEqual(ctx.exception.code, ErrorCode.NON_FINITE_FEATURE)
            self.assertEqual(
                router.metrics_snapshot()["errors_by_code"].get("NON_FINITE_FEATURE"), 1
            )
        finally:
            router.close()

    def test_scenario_4_invalid_artifact_rejected(self) -> None:
        router = InferenceRouter(runtime="cpp_native")
        try:
            with tempfile.TemporaryDirectory() as td:
                # Tampered weights.bin
                tampered = Path(td) / "tampered_v1"
                shutil.copytree(V1_DIR, tampered)
                w = bytearray((tampered / "weights.bin").read_bytes())
                w[-1] ^= 0x55
                (tampered / "weights.bin").write_bytes(bytes(w))
                with self.assertRaises(EngineError) as ctx_t:
                    router.load_version(tampered, activate_as_primary=True)
                self.assertEqual(ctx_t.exception.code, ErrorCode.ARTIFACT_INTEGRITY_ERROR)

                # Untrusted pickle file
                pkl = Path(td) / "model.pkl"
                pkl.write_bytes(b"pickle-bytes")
                with self.assertRaises(EngineError) as ctx_p:
                    router.load_version(pkl, activate_as_primary=True)
                self.assertEqual(ctx_p.exception.code, ErrorCode.UNTRUSTED_ARTIFACT_FORMAT)

            self.assertEqual(router.metrics_snapshot()["reload_failures"], 2)
        finally:
            router.close()

    def test_scenario_5_model_reload_and_safe_failure_isolation(self) -> None:
        router = InferenceRouter(runtime="cpp_native")
        try:
            router.load_version(V1_DIR, activate_as_primary=True)
            r_v1 = router.predict(self._make_req("sc5-v1"))
            self.assertEqual(r_v1.model_version, "1.0.0")

            # Hot atomic reload to v2.0.0
            router.load_version(V2_DIR, activate_as_primary=True)
            r_v2 = router.predict(self._make_req("sc5-v2"))
            self.assertEqual(r_v2.model_version, "2.0.0")
            self.assertNotAlmostEqual(r_v1.prediction, r_v2.prediction, places=6)

            # Attempt reload with corrupted bundle -> must fail safely without disrupting v2.0.0
            with tempfile.TemporaryDirectory() as td:
                corrupt = Path(td) / "corrupt_v3"
                shutil.copytree(V2_DIR, corrupt)
                (corrupt / "weights.bin").write_bytes(b"corrupted")
                with self.assertRaises(EngineError) as ctx:
                    router.load_version(corrupt, activate_as_primary=True)
                self.assertEqual(ctx.exception.code, ErrorCode.ARTIFACT_INTEGRITY_ERROR)

            # Active model is still v2.0.0
            r_still_v2 = router.predict(self._make_req("sc5-still-v2"))
            self.assertEqual(r_still_v2.model_version, "2.0.0")

            # Rollback cleanly restores v1.0.0
            router.rollback()
            r_rb = router.predict(self._make_req("sc5-rollback"))
            self.assertEqual(r_rb.model_version, "1.0.0")
            self.assertAlmostEqual(r_rb.prediction, r_v1.prediction, places=12)
        finally:
            router.close()

    def test_scenario_6_overload_shedding(self) -> None:
        entered = threading.Event()
        release = threading.Event()

        def slow_hook() -> None:
            entered.set()
            release.wait(timeout=2.0)

        router = InferenceRouter(
            runtime="cpp_native",
            max_in_flight=1,
            pre_score_hook=slow_hook,
        )
        try:
            router.load_version(V1_DIR, activate_as_primary=True)
            with ThreadPoolExecutor(max_workers=2) as pool:
                f1 = pool.submit(router.predict, self._make_req("sc6-inflight-1"))
                self.assertTrue(entered.wait(timeout=1.0))

                # Second concurrent call must be shed immediately with OVERLOAD_REJECTED
                with self.assertRaises(EngineError) as ctx:
                    router.predict(self._make_req("sc6-shed-2"))
                self.assertEqual(ctx.exception.code, ErrorCode.OVERLOAD_REJECTED)

                release.set()
                resp1 = f1.result(timeout=2.0)
                self.assertEqual(resp1.model_version, "1.0.0")

            self.assertEqual(router.metrics_snapshot()["overload_rejections"], 1)
        finally:
            release.set()
            router.close()

    def test_scenario_7_cold_start_vs_warm(self) -> None:
        cold = measure_cold_start(V1_DIR, "cpp_native", self.valid_features)
        self.assertGreater(cold["total_cold_start_us"], 0.0)
        self.assertGreater(cold["load_and_verify_us"], 0.0)

        # Measure warm calls and confirm cold start includes load+verify+parity overhead
        cpp = CppNativeAdapter(self.art_v1)
        try:
            warm_lats = []
            req = self._make_req("sc7-warm")
            for _ in range(50):
                cpp.score_request(req)
            for _ in range(200):
                t0 = time.perf_counter_ns()
                cpp.score_request(req)
                warm_lats.append((time.perf_counter_ns() - t0) / 1000.0)
            warm_stats = compute_latency_stats(warm_lats)
            self.assertGreater(cold["total_cold_start_us"], warm_stats["p50_us"])
        finally:
            cpp.close()

    def test_scenario_8_latency_tail_regression_detected(self) -> None:
        cpp = CppNativeAdapter(self.art_v1)
        try:
            req = self._make_req("sc8-reg")
            baseline_lats = []
            for _ in range(200):
                t0 = time.perf_counter_ns()
                cpp.score_request(req)
                baseline_lats.append((time.perf_counter_ns() - t0) / 1000.0)
            baseline = compute_latency_stats(baseline_lats)

            # Inject a tail latency spike on 5% of requests (e.g., GC pause or lock contention)
            regressed_lats = []
            for i in range(200):
                t0 = time.perf_counter_ns()
                cpp.score_request(req)
                if i % 20 == 0:
                    time.sleep(0.002)  # 2000 us tail spike
                regressed_lats.append((time.perf_counter_ns() - t0) / 1000.0)
            regressed = compute_latency_stats(regressed_lats)

            gate = check_latency_tail_regression(
                regressed,
                baseline_p99_us=baseline["p99_us"],
                max_regression_ratio=2.5,
                absolute_p99_cap_us=500.0,
            )
            self.assertFalse(gate["passed"])
            self.assertGreaterEqual(len(gate["failures"]), 1)
        finally:
            cpp.close()


if __name__ == "__main__":
    unittest.main()
