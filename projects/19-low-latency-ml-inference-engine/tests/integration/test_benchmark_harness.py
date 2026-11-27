"""Integration tests for the latency and load benchmark harness."""

from __future__ import annotations

import unittest
from pathlib import Path

from inference_engine.benchmark import (
    check_latency_tail_regression,
    render_benchmark_markdown,
    run_full_benchmark_suite,
)


PROJECT_ROOT = Path(__file__).resolve().parents[2]
V1_DIR = PROJECT_ROOT / "data" / "fixtures" / "models" / "lob_alpha_mlp" / "v1.0.0"


class TestBenchmarkHarness(unittest.TestCase):
    def test_full_suite_produces_percentiles_environment_and_markdown(self) -> None:
        report = run_full_benchmark_suite(
            V1_DIR,
            warmup_iters=20,
            single_iters=200,
            batch_size=16,
            batch_iters=20,
            concurrent_threads=2,
            concurrent_per_thread=50,
        )
        self.assertIn("environment", report)
        self.assertIn("python_version", report["environment"])
        ws = report["warm_single"]["cpp_native_preallocated"]
        self.assertEqual(ws["count"], 200)
        self.assertLessEqual(ws["p50_us"], ws["p95_us"])
        self.assertLessEqual(ws["p95_us"], ws["p99_us"])
        self.assertIsNotNone(report["pure_cpp20_native_binary"])
        md = render_benchmark_markdown(report)
        self.assertIn("## Warm Single-Request Inference", md)
        self.assertIn("p99", md)

    def test_tail_regression_detector_flags_regression(self) -> None:
        ok = check_latency_tail_regression(
            {"p99_us": 40.0}, baseline_p99_us=30.0, max_regression_ratio=2.0, absolute_p99_cap_us=100.0
        )
        self.assertTrue(ok["passed"])

        bad_ratio = check_latency_tail_regression(
            {"p99_us": 80.0}, baseline_p99_us=30.0, max_regression_ratio=2.0, absolute_p99_cap_us=100.0
        )
        self.assertFalse(bad_ratio["passed"])
        self.assertEqual(len(bad_ratio["failures"]), 1)

        bad_cap = check_latency_tail_regression(
            {"p99_us": 150.0}, baseline_p99_us=100.0, max_regression_ratio=2.0, absolute_p99_cap_us=100.0
        )
        self.assertFalse(bad_cap["passed"])


if __name__ == "__main__":
    unittest.main()
