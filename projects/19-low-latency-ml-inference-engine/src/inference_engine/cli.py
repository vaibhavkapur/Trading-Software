"""Command-line interface for fixture generation, parity verification, benchmarking, and demos."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional

from .artifact import load_verified_artifact
from .benchmark import render_benchmark_markdown, run_full_benchmark_suite
from .contracts import EngineError, InferenceRequest, format_utc_timestamp, utc_now
from .reference_scorer import build_fixtures_and_models
from .router import InferenceRouter, RoutingPolicy
from .runtime_adapter import CppNativeAdapter, PythonReferenceAdapter


def _project_root() -> Path:
    return Path(__file__).resolve().parent.parent.parent


def cmd_build_fixtures(args: argparse.Namespace) -> int:
    root = Path(args.root).resolve()
    summary = build_fixtures_and_models(root)
    reports_dir = root / "reports"
    reports_dir.mkdir(parents=True, exist_ok=True)
    out_path = reports_dir / "m1_foundation_validation.json"
    out_path.write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"Generated synthetic fixtures and verified models; report written to {out_path}")
    return 0


def cmd_verify_parity(args: argparse.Namespace) -> int:
    root = Path(args.root).resolve()
    models_root = root / "data" / "fixtures" / "models" / "lob_alpha_mlp"
    results: List[Dict[str, Any]] = []
    for version in ("1.0.0", "2.0.0"):
        bdir = models_root / f"v{version}"
        art = load_verified_artifact(bdir, verify_sha256=True)
        py_adapter = PythonReferenceAdapter(art)
        cpp_adapter = CppNativeAdapter(art)
        try:
            py_res = py_adapter.verify_parity()
            cpp_res = cpp_adapter.verify_parity()
            # Cross-check exact Python vs C++ parity on every golden vector
            max_cross_err = 0.0
            for gv in art.golden_vectors:
                raw = [gv.raw_features[f.name] for f in art.manifest.features]
                p_py = py_adapter.score_raw_one(raw)
                p_cpp = cpp_adapter.score_raw_one(raw)
                err = abs(p_py - p_cpp)
                if err > max_cross_err:
                    max_cross_err = err
            tol = art.manifest.output.prediction_tolerance_abs
            if max_cross_err > tol:
                raise RuntimeError(f"Cross parity failed for v{version}: {max_cross_err} > {tol}")
            results.append(
                {
                    "model_id": art.manifest.model_id,
                    "model_version": version,
                    "artifact_sha256": art.bundle_sha256,
                    "python_golden_parity": py_res,
                    "cpp_golden_parity": cpp_res,
                    "python_vs_cpp_max_abs_error": max_cross_err,
                    "tolerance_abs": tol,
                    "passed": True,
                }
            )
        finally:
            cpp_adapter.close()
    reports_dir = root / "reports"
    reports_dir.mkdir(parents=True, exist_ok=True)
    out_path = reports_dir / "m2_parity_validation.json"
    payload = {
        "timestamp_utc": format_utc_timestamp(utc_now()),
        "verified_models": results,
        "all_passed": all(r["passed"] for r in results),
    }
    out_path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps(payload, indent=2))
    return 0


def cmd_benchmark(args: argparse.Namespace) -> int:
    root = Path(args.root).resolve()
    cfg_path = root / "config" / "benchmark_targets.json"
    cfg = json.loads(cfg_path.read_text(encoding="utf-8"))
    sc = cfg["sample_counts"]
    gates = cfg["regression_gates_us"]

    bundle_dir = root / "data" / "fixtures" / "models" / "lob_alpha_mlp" / f"v{args.version}"
    report = run_full_benchmark_suite(
        bundle_dir,
        warmup_iters=args.warmup or sc["warmup_iterations"],
        single_iters=args.single_iters or sc["single_iterations"],
        batch_size=sc["batch_size"],
        batch_iters=args.batch_iters or sc["batch_iterations"],
        concurrent_threads=sc["concurrent_threads"],
        concurrent_per_thread=args.concurrent_iters or sc["concurrent_requests_per_thread"],
        gates_us=gates,
    )
    reports_dir = root / "reports"
    reports_dir.mkdir(parents=True, exist_ok=True)
    json_path = reports_dir / "m3_benchmark_report.json"
    md_path = reports_dir / "m3_benchmark_report.md"
    json_path.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    md_path.write_text(render_benchmark_markdown(report), encoding="utf-8")
    print(f"Benchmark JSON written to {json_path}")
    print(f"Benchmark Markdown written to {md_path}")
    print(
        f"Summary: cpp_native single p50={report['warm_single']['cpp_native_preallocated']['p50_us']:.2f}us "
        f"p99={report['warm_single']['cpp_native_preallocated']['p99_us']:.2f}us "
        f"(all_gates_passed={report['all_gates_passed']})"
    )
    return 0 if report["all_gates_passed"] else 1


def cmd_demo(args: argparse.Namespace) -> int:
    root = Path(args.root).resolve()
    models_root = root / "data" / "fixtures" / "models" / "lob_alpha_mlp"
    v1_dir = models_root / "v1.0.0"
    v2_dir = models_root / "v2.0.0"

    router = InferenceRouter(
        runtime="cpp_native",
        max_in_flight=8,
        order_execution_enabled=False,
    )
    try:
        ev1 = router.load_version(v1_dir, activate_as_primary=True)
        ev2 = router.load_version(v2_dir, activate_as_primary=False)

        art_v1 = load_verified_artifact(v1_dir)
        sample_features = art_v1.golden_vectors[0].raw_features
        now_ts = format_utc_timestamp(utc_now())

        req_primary = InferenceRequest(
            request_id="demo-req-001",
            instrument_id="SYN-EQUITY-A",
            currency="USD",
            event_ts_utc=now_ts,
            available_ts_utc=now_ts,
            inference_ts_utc=now_ts,
            features=sample_features,
        )
        resp_v1 = router.predict(req_primary)

        # Enable 50% canary routing between v1.0.0 and v2.0.0
        router.set_routing_policy(
            RoutingPolicy(primary_version="1.0.0", canary_version="2.0.0", canary_weight=0.5)
        )
        canary_versions = []
        for i in range(10):
            r = router.predict(
                InferenceRequest(
                    request_id=f"demo-canary-{i:03d}",
                    instrument_id="SYN-EQUITY-A",
                    currency="USD",
                    event_ts_utc=now_ts,
                    available_ts_utc=now_ts,
                    inference_ts_utc=now_ts,
                    features=sample_features,
                )
            )
            canary_versions.append(r.model_version)

        # Promote v2.0.0 then roll back to canary, then roll back to v1.0.0
        router.set_routing_policy(RoutingPolicy(primary_version="2.0.0"))
        resp_v2 = router.predict(req_primary)
        rolled_back_1 = router.rollback()
        rolled_back_2 = router.rollback()

        # Demonstrate safe failure on stale feature & NaN
        safe_failures = {}
        try:
            router.predict(
                InferenceRequest(
                    request_id="demo-stale",
                    instrument_id="SYN-EQUITY-A",
                    currency="USD",
                    event_ts_utc="2026-01-15T14:00:00.000Z",
                    available_ts_utc="2026-01-15T14:00:00.005Z",
                    inference_ts_utc=now_ts,
                    features=sample_features,
                )
            )
        except EngineError as exc:
            safe_failures["stale_feature"] = exc.to_dict()

        bad_features = dict(sample_features)
        bad_features["order_book_imbalance_l1"] = float("nan")
        try:
            router.predict(
                InferenceRequest(
                    request_id="demo-nan",
                    instrument_id="SYN-EQUITY-A",
                    currency="USD",
                    event_ts_utc=now_ts,
                    available_ts_utc=now_ts,
                    inference_ts_utc=now_ts,
                    features=bad_features,
                )
            )
        except EngineError as exc:
            safe_failures["nan_feature"] = exc.to_dict()

        acceptance = {
            "timestamp_utc": format_utc_timestamp(utc_now()),
            "loaded": [ev1, ev2],
            "v1_prediction": resp_v1.to_dict(),
            "canary_sample_versions": canary_versions,
            "v2_prediction": resp_v2.to_dict(),
            "rollback_step_1_primary": rolled_back_1.primary_version,
            "rollback_step_2_primary": rolled_back_2.primary_version,
            "safe_failures": safe_failures,
            "health": router.health_snapshot(),
            "metrics": router.metrics_snapshot(),
        }
        reports_dir = root / "reports"
        reports_dir.mkdir(parents=True, exist_ok=True)
        out_path = reports_dir / "m4_hardening_acceptance.json"
        out_path.write_text(json.dumps(acceptance, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        print(json.dumps(acceptance, indent=2))
        return 0
    finally:
        router.close()


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="ml-inference-engine",
        description="Low-Latency ML Inference Engine CLI",
    )
    parser.add_argument("--root", default=str(_project_root()), help="Project root directory")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("build-fixtures", help="Generate synthetic LOB fixtures and export v1/v2 models")
    sub.add_parser("verify-parity", help="Verify exact Python vs C++20 native parity across models")

    p_bench = sub.add_parser("benchmark", help="Run cold/warm/batch/concurrent benchmark harness")
    p_bench.add_argument("--version", default="1.0.0", help="Model version to benchmark")
    p_bench.add_argument("--warmup", type=int, default=0)
    p_bench.add_argument("--single-iters", type=int, default=0)
    p_bench.add_argument("--batch-iters", type=int, default=0)
    p_bench.add_argument("--concurrent-iters", type=int, default=0)

    sub.add_parser("demo", help="Run end-to-end rollout, canary, rollback, and fail-safe demo")

    args = parser.parse_args(argv)
    if args.command == "build-fixtures":
        return cmd_build_fixtures(args)
    if args.command == "verify-parity":
        return cmd_verify_parity(args)
    if args.command == "benchmark":
        return cmd_benchmark(args)
    if args.command == "demo":
        return cmd_demo(args)
    return 2


if __name__ == "__main__":
    sys.exit(main())
