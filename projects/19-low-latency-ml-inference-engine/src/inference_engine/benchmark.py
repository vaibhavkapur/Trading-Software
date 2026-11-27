"""Load and latency benchmark harness with cold/warm, single/batch, concurrent, and regression checks."""

from __future__ import annotations

import json
import math
import os
import platform
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Sequence

from .artifact import load_verified_artifact
from .contracts import (
    BatchInferenceRequest,
    EngineError,
    ErrorCode,
    InferenceRequest,
    format_utc_timestamp,
    utc_now,
)
from .runtime_adapter import (
    CppNativeAdapter,
    PythonReferenceAdapter,
    default_native_library_path,
)


def compute_latency_stats(samples_us: Sequence[float]) -> Dict[str, Any]:
    """Compute exact summary statistics and percentiles in microseconds."""
    if not samples_us:
        return {
            "count": 0,
            "mean_us": 0.0,
            "min_us": 0.0,
            "p50_us": 0.0,
            "p95_us": 0.0,
            "p99_us": 0.0,
            "max_us": 0.0,
        }
    arr = sorted(float(x) for x in samples_us)
    n = len(arr)

    def _pct(p: float) -> float:
        idx = p * (n - 1)
        lo = int(math.floor(idx))
        hi = int(math.ceil(idx))
        if lo == hi:
            return arr[lo]
        frac = idx - lo
        return arr[lo] * (1.0 - frac) + arr[hi] * frac

    return {
        "count": n,
        "mean_us": round(sum(arr) / n, 4),
        "min_us": round(arr[0], 4),
        "p50_us": round(_pct(0.50), 4),
        "p95_us": round(_pct(0.95), 4),
        "p99_us": round(_pct(0.99), 4),
        "max_us": round(arr[-1], 4),
    }


def collect_environment_info() -> Dict[str, Any]:
    """Capture host environment metadata so latency numbers are never reported without context."""
    return {
        "timestamp_utc": format_utc_timestamp(utc_now()),
        "os": platform.system(),
        "os_release": platform.release(),
        "machine": platform.machine(),
        "processor": platform.processor() or platform.machine(),
        "cpu_count": os.cpu_count(),
        "python_version": sys.version.split()[0],
    }


def check_latency_tail_regression(
    measured_stats: Dict[str, Any],
    *,
    baseline_p99_us: Optional[float] = None,
    max_regression_ratio: float = 2.5,
    absolute_p99_cap_us: Optional[float] = None,
) -> Dict[str, Any]:
    """Evaluate measured p99 latency against a declared baseline ratio and/or absolute cap."""
    p99 = float(measured_stats["p99_us"])
    failures: List[str] = []
    ratio: Optional[float] = None

    if baseline_p99_us is not None and baseline_p99_us > 0.0:
        ratio = p99 / baseline_p99_us
        if ratio > max_regression_ratio:
            failures.append(
                f"p99 ({p99:.3f} us) regressed {ratio:.2f}x over baseline ({baseline_p99_us:.3f} us), limit {max_regression_ratio:.2f}x"
            )
    if absolute_p99_cap_us is not None and p99 > absolute_p99_cap_us:
        failures.append(
            f"p99 ({p99:.3f} us) exceeded absolute cap ({absolute_p99_cap_us:.3f} us)"
        )

    return {
        "passed": len(failures) == 0,
        "measured_p99_us": p99,
        "baseline_p99_us": baseline_p99_us,
        "regression_ratio": round(ratio, 4) if ratio is not None else None,
        "max_regression_ratio": max_regression_ratio,
        "absolute_p99_cap_us": absolute_p99_cap_us,
        "failures": failures,
    }


def _make_sample_request(idx: int, features: Dict[str, float], ts: str) -> InferenceRequest:
    return InferenceRequest(
        request_id=f"bench-req-{idx:06d}",
        instrument_id="SYN-EQUITY-A",
        currency="USD",
        event_ts_utc=ts,
        available_ts_utc=ts,
        inference_ts_utc=ts,
        features=features,
    )


def measure_cold_start(
    bundle_dir: Path,
    runtime: str,
    sample_features: Dict[str, float],
    *,
    library_path: Optional[Path] = None,
) -> Dict[str, Any]:
    """Measure cold artifact load + SHA-256 verify + adapter init + parity check + first inference."""
    ts = "2026-01-15T15:00:00.000Z"
    req = _make_sample_request(0, sample_features, ts)

    t0 = time.perf_counter_ns()
    artifact = load_verified_artifact(bundle_dir, verify_sha256=True)
    t1 = time.perf_counter_ns()
    if runtime == "cpp_native":
        adapter = CppNativeAdapter(artifact, library_path=library_path)
    else:
        adapter = PythonReferenceAdapter(artifact)
    t2 = time.perf_counter_ns()
    adapter.verify_parity()
    t3 = time.perf_counter_ns()
    resp = adapter.score_request(req)
    t4 = time.perf_counter_ns()
    adapter.close()

    return {
        "runtime": runtime,
        "model_version": artifact.manifest.model_version,
        "load_and_verify_us": round((t1 - t0) / 1000.0, 4),
        "adapter_init_us": round((t2 - t1) / 1000.0, 4),
        "golden_parity_check_us": round((t3 - t2) / 1000.0, 4),
        "first_inference_us": round((t4 - t3) / 1000.0, 4),
        "total_cold_start_us": round((t4 - t0) / 1000.0, 4),
        "first_prediction": resp.prediction,
    }


def _benchmark_single_adapter(
    adapter_fn: Callable[[InferenceRequest], Any],
    requests: Sequence[InferenceRequest],
    warmup_iters: int,
    single_iters: int,
) -> Dict[str, Any]:
    n_req = len(requests)
    for i in range(warmup_iters):
        adapter_fn(requests[i % n_req])

    lat_us: List[float] = []
    t_wall_0 = time.perf_counter_ns()
    for i in range(single_iters):
        req = requests[i % n_req]
        t0 = time.perf_counter_ns()
        adapter_fn(req)
        t1 = time.perf_counter_ns()
        lat_us.append((t1 - t0) / 1000.0)
    wall_s = (time.perf_counter_ns() - t_wall_0) / 1e9
    stats = compute_latency_stats(lat_us)
    stats["throughput_qps"] = round(single_iters / wall_s, 2) if wall_s > 0 else 0.0
    return stats


def _benchmark_batch_adapter(
    adapter_batch_fn: Callable[[BatchInferenceRequest], Any],
    batch_req: BatchInferenceRequest,
    warmup_iters: int,
    batch_iters: int,
) -> Dict[str, Any]:
    for _ in range(max(5, warmup_iters // 10)):
        adapter_batch_fn(batch_req)

    batch_size = len(batch_req.requests)
    batch_lats: List[float] = []
    per_item_lats: List[float] = []
    t_wall_0 = time.perf_counter_ns()
    for _ in range(batch_iters):
        t0 = time.perf_counter_ns()
        adapter_batch_fn(batch_req)
        t1 = time.perf_counter_ns()
        dt = (t1 - t0) / 1000.0
        batch_lats.append(dt)
        per_item_lats.append(dt / batch_size)
    wall_s = (time.perf_counter_ns() - t_wall_0) / 1e9
    return {
        "batch_size": batch_size,
        "batch_latency": compute_latency_stats(batch_lats),
        "per_item_latency": compute_latency_stats(per_item_lats),
        "items_per_sec": round((batch_iters * batch_size) / wall_s, 2) if wall_s > 0 else 0.0,
    }


def _benchmark_concurrent(
    adapter_fn: Callable[[InferenceRequest], Any],
    requests: Sequence[InferenceRequest],
    num_threads: int,
    per_thread_iters: int,
) -> Dict[str, Any]:
    n_req = len(requests)

    def _worker(tid: int) -> List[float]:
        lats: List[float] = []
        offset = tid * 17
        for i in range(per_thread_iters):
            req = requests[(offset + i) % n_req]
            t0 = time.perf_counter_ns()
            adapter_fn(req)
            t1 = time.perf_counter_ns()
            lats.append((t1 - t0) / 1000.0)
        return lats

    t_wall_0 = time.perf_counter_ns()
    with ThreadPoolExecutor(max_workers=num_threads) as pool:
        futures = [pool.submit(_worker, tid) for tid in range(num_threads)]
        all_lats: List[float] = []
        for f in futures:
            all_lats.extend(f.result())
    wall_s = (time.perf_counter_ns() - t_wall_0) / 1e9
    stats = compute_latency_stats(all_lats)
    stats["threads"] = num_threads
    stats["throughput_qps"] = round(len(all_lats) / wall_s, 2) if wall_s > 0 else 0.0
    return stats


def run_native_binary_benchmark(
    weights_path: Path,
    *,
    bench_bin: Optional[Path] = None,
    single_iters: int = 20000,
    batch_iters: int = 1000,
    threads: int = 4,
) -> Optional[Dict[str, Any]]:
    """Invoke compiled C++20 bench_native binary if available."""
    bin_path = (
        Path(bench_bin)
        if bench_bin is not None
        else Path(__file__).resolve().parent.parent / "native" / "build" / "bench_native"
    )
    if not bin_path.is_file():
        return None
    proc = subprocess.run(
        [
            str(bin_path),
            str(weights_path),
            str(single_iters),
            str(batch_iters),
            str(threads),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(proc.stdout.strip())


def run_full_benchmark_suite(
    bundle_dir: Path,
    *,
    warmup_iters: int = 200,
    single_iters: int = 3000,
    batch_size: int = 64,
    batch_iters: int = 300,
    concurrent_threads: int = 4,
    concurrent_per_thread: int = 500,
    gates_us: Optional[Dict[str, float]] = None,
) -> Dict[str, Any]:
    """Run complete M3 benchmark suite across cold/warm, single/batch, concurrent, and allocation modes."""
    bundle_path = Path(bundle_dir)
    artifact = load_verified_artifact(bundle_path, verify_sha256=True)
    ts = "2026-01-15T15:00:00.000Z"

    sample_requests = [
        _make_sample_request(i, gv.raw_features, ts)
        for i, gv in enumerate(artifact.golden_vectors)
    ]
    batch_items = [sample_requests[i % len(sample_requests)] for i in range(batch_size)]
    batch_req = BatchInferenceRequest(batch_id="bench-batch-001", requests=batch_items)

    # 1. Cold start measurements
    cold_py = measure_cold_start(bundle_path, "python_reference", sample_requests[0].features)
    cold_cpp = measure_cold_start(bundle_path, "cpp_native", sample_requests[0].features)

    # 2. Warm single & batch — Python Reference
    py_adapter = PythonReferenceAdapter(artifact)
    py_single = _benchmark_single_adapter(
        py_adapter.score_request, sample_requests, warmup_iters, single_iters
    )
    py_batch = _benchmark_batch_adapter(
        py_adapter.score_batch, batch_req, warmup_iters, batch_iters
    )

    # 3. Warm single — C++20 native BEFORE vs AFTER pre-allocated buffer optimization
    cpp_unopt = CppNativeAdapter(artifact, use_preallocated_buffers=False)
    cpp_unopt_single = _benchmark_single_adapter(
        cpp_unopt.score_request, sample_requests, warmup_iters, single_iters
    )
    cpp_unopt.close()

    cpp_opt = CppNativeAdapter(artifact, use_preallocated_buffers=True)
    cpp_opt_single = _benchmark_single_adapter(
        cpp_opt.score_request, sample_requests, warmup_iters, single_iters
    )
    cpp_opt_batch = _benchmark_batch_adapter(
        cpp_opt.score_batch, batch_req, warmup_iters, batch_iters
    )
    cpp_opt_conc = _benchmark_concurrent(
        cpp_opt.score_request, sample_requests, concurrent_threads, concurrent_per_thread
    )
    kernel_version = cpp_opt.kernel_version
    cpp_opt.close()

    # 4. Pure C++20 native kernel benchmark (no Python wrapper overhead)
    native_bin_stats = run_native_binary_benchmark(
        bundle_path / artifact.manifest.weights_file,
        single_iters=max(10000, single_iters),
        batch_iters=max(500, batch_iters),
        threads=concurrent_threads,
    )

    # 5. Regression gate evaluation
    gates = gates_us or {}
    gate_checks = {
        "cpp_native_single_p99": check_latency_tail_regression(
            cpp_opt_single,
            absolute_p99_cap_us=gates.get("cpp_native_single_p99_max_us", 500.0),
        ),
        "cpp_native_batch_per_item_p99": check_latency_tail_regression(
            cpp_opt_batch["per_item_latency"],
            absolute_p99_cap_us=gates.get("cpp_native_batch_per_item_p99_max_us", 100.0),
        ),
        "python_reference_single_p99": check_latency_tail_regression(
            py_single,
            absolute_p99_cap_us=gates.get("python_reference_single_p99_max_us", 2000.0),
        ),
    }

    speedup_vs_python_p50 = (
        round(py_single["p50_us"] / cpp_opt_single["p50_us"], 2)
        if cpp_opt_single["p50_us"] > 0
        else None
    )
    alloc_opt_delta_pct = (
        round(
            (cpp_unopt_single["p50_us"] - cpp_opt_single["p50_us"])
            / cpp_unopt_single["p50_us"]
            * 100.0,
            2,
        )
        if cpp_unopt_single["p50_us"] > 0
        else 0.0
    )

    return {
        "environment": collect_environment_info(),
        "model": {
            "model_id": artifact.manifest.model_id,
            "model_version": artifact.manifest.model_version,
            "artifact_sha256": artifact.bundle_sha256,
            "kernel_version": kernel_version,
            "native_library": str(default_native_library_path()),
        },
        "cold_start": {
            "python_reference": cold_py,
            "cpp_native": cold_cpp,
        },
        "warm_single": {
            "python_reference": py_single,
            "cpp_native_per_call_alloc": cpp_unopt_single,
            "cpp_native_preallocated": cpp_opt_single,
            "p50_speedup_cpp_over_python": speedup_vs_python_p50,
            "preallocation_p50_improvement_pct": alloc_opt_delta_pct,
        },
        "warm_batch": {
            "python_reference": py_batch,
            "cpp_native_preallocated": cpp_opt_batch,
        },
        "concurrent_python_cpp_adapter": cpp_opt_conc,
        "pure_cpp20_native_binary": native_bin_stats,
        "regression_gates": gate_checks,
        "all_gates_passed": all(g["passed"] for g in gate_checks.values()),
    }


def render_benchmark_markdown(report: Dict[str, Any]) -> str:
    """Format benchmark report JSON into a clear, evidence-backed Markdown report."""
    env = report["environment"]
    ws = report["warm_single"]
    wb = report["warm_batch"]
    conc = report["concurrent_python_cpp_adapter"]
    cold = report["cold_start"]
    native = report.get("pure_cpp20_native_binary")

    lines = [
        "# M3 Latency & Load Benchmark Report",
        "",
        "> Measured on local hardware with synthetic LOB fixtures. Never extrapolate local benchmark results to live production SLAs without target-hardware validation.",
        "",
        "## Environment",
        "",
        "| Field | Value |",
        "|---|---|",
        f"| Timestamp (UTC) | `{env['timestamp_utc']}` |",
        f"| OS / Arch | `{env['os']} {env['os_release']} ({env['machine']})` |",
        f"| CPU cores | `{env['cpu_count']}` |",
        f"| Python | `{env['python_version']}` |",
        f"| Model | `{report['model']['model_id']} v{report['model']['model_version']}` |",
        f"| Artifact SHA-256 | `{report['model']['artifact_sha256'][:16]}...` |",
        f"| Native kernel | `{report['model']['kernel_version']}` |",
        "",
        "## Cold Start (Load + SHA-256 Verify + Init + Parity + First Inference)",
        "",
        "| Runtime | Load & Verify (us) | Adapter Init (us) | Golden Parity (us) | First Inference (us) | Total Cold Start (us) |",
        "|---|---:|---:|---:|---:|---:|",
        f"| `python_reference` | {cold['python_reference']['load_and_verify_us']:.2f} | {cold['python_reference']['adapter_init_us']:.2f} | {cold['python_reference']['golden_parity_check_us']:.2f} | {cold['python_reference']['first_inference_us']:.2f} | {cold['python_reference']['total_cold_start_us']:.2f} |",
        f"| `cpp_native` | {cold['cpp_native']['load_and_verify_us']:.2f} | {cold['cpp_native']['adapter_init_us']:.2f} | {cold['cpp_native']['golden_parity_check_us']:.2f} | {cold['cpp_native']['first_inference_us']:.2f} | {cold['cpp_native']['total_cold_start_us']:.2f} |",
        "",
        "## Warm Single-Request Inference (End-to-End Python API)",
        "",
        "| Path | Samples | p50 (us) | p95 (us) | p99 (us) | Max (us) | Throughput (QPS) |",
        "|---|---:|---:|---:|---:|---:|---:|",
        f"| `python_reference` | {ws['python_reference']['count']} | {ws['python_reference']['p50_us']:.2f} | {ws['python_reference']['p95_us']:.2f} | {ws['python_reference']['p99_us']:.2f} | {ws['python_reference']['max_us']:.2f} | {ws['python_reference']['throughput_qps']:.1f} |",
        f"| `cpp_native` (per-call alloc) | {ws['cpp_native_per_call_alloc']['count']} | {ws['cpp_native_per_call_alloc']['p50_us']:.2f} | {ws['cpp_native_per_call_alloc']['p95_us']:.2f} | {ws['cpp_native_per_call_alloc']['p99_us']:.2f} | {ws['cpp_native_per_call_alloc']['max_us']:.2f} | {ws['cpp_native_per_call_alloc']['throughput_qps']:.1f} |",
        f"| `cpp_native` (pre-allocated TLS) | {ws['cpp_native_preallocated']['count']} | {ws['cpp_native_preallocated']['p50_us']:.2f} | {ws['cpp_native_preallocated']['p95_us']:.2f} | {ws['cpp_native_preallocated']['p99_us']:.2f} | {ws['cpp_native_preallocated']['max_us']:.2f} | {ws['cpp_native_preallocated']['throughput_qps']:.1f} |",
        "",
        f"- **p50 speedup (`cpp_native` pre-allocated vs `python_reference`)**: `{ws['p50_speedup_cpp_over_python']}x`",
        f"- **Pre-allocated TLS buffer p50 change vs per-call ctypes allocation**: `{ws['preallocation_p50_improvement_pct']}%`",
        "",
        "## Warm Batch Inference (Batch Size 64)",
        "",
        "| Path | Batches | Batch p50 (us) | Batch p99 (us) | Per-Item p50 (us) | Per-Item p99 (us) | Items/sec |",
        "|---|---:|---:|---:|---:|---:|---:|",
        f"| `python_reference` | {wb['python_reference']['batch_latency']['count']} | {wb['python_reference']['batch_latency']['p50_us']:.2f} | {wb['python_reference']['batch_latency']['p99_us']:.2f} | {wb['python_reference']['per_item_latency']['p50_us']:.2f} | {wb['python_reference']['per_item_latency']['p99_us']:.2f} | {wb['python_reference']['items_per_sec']:.1f} |",
        f"| `cpp_native` | {wb['cpp_native_preallocated']['batch_latency']['count']} | {wb['cpp_native_preallocated']['batch_latency']['p50_us']:.2f} | {wb['cpp_native_preallocated']['batch_latency']['p99_us']:.2f} | {wb['cpp_native_preallocated']['per_item_latency']['p50_us']:.2f} | {wb['cpp_native_preallocated']['per_item_latency']['p99_us']:.2f} | {wb['cpp_native_preallocated']['items_per_sec']:.1f} |",
        "",
        "## Concurrent Load (Python ThreadPool -> C++20 Native Adapter)",
        "",
        f"- Threads: `{conc['threads']}`, Total Requests: `{conc['count']}`, Throughput: `{conc['throughput_qps']:.1f} QPS`",
        f"- Latency: p50=`{conc['p50_us']:.2f} us`, p95=`{conc['p95_us']:.2f} us`, p99=`{conc['p99_us']:.2f} us`, max=`{conc['max_us']:.2f} us`",
        "",
    ]

    if native:
        ns = native["warm_single"]
        nb = native["warm_batch_64_per_item"]
        nc = native["concurrent"]
        lines.extend(
            [
                "## Pure C++20 Native Kernel Benchmark (No Python Boundary Overhead)",
                "",
                f"- Cold start (read weights + init + first score): `{native['cold_start_us']:.2f} us`",
                "",
                "| Workload | Samples | p50 (us) | p95 (us) | p99 (us) | Max (us) |",
                "|---|---:|---:|---:|---:|---:|",
                f"| Single inference | {ns['count']} | {ns['p50_us']:.4f} | {ns['p95_us']:.4f} | {ns['p99_us']:.4f} | {ns['max_us']:.4f} |",
                f"| Batch-64 per-item | {nb['count']} | {nb['p50_us']:.4f} | {nb['p95_us']:.4f} | {nb['p99_us']:.4f} | {nb['max_us']:.4f} |",
                f"| Concurrent ({nc['threads']} threads, {nc['throughput_qps']:.0f} QPS) | {nc['latency']['count']} | {nc['latency']['p50_us']:.4f} | {nc['latency']['p95_us']:.4f} | {nc['latency']['p99_us']:.4f} | {nc['latency']['max_us']:.4f} |",
                "",
            ]
        )

    lines.extend(
        [
            "## Regression Gates",
            "",
            "| Gate | Measured p99 (us) | Cap (us) | Passed |",
            "|---|---:|---:|---|",
        ]
    )
    for name, gate in report["regression_gates"].items():
        lines.append(
            f"| `{name}` | {gate['measured_p99_us']:.2f} | {gate['absolute_p99_cap_us']:.2f} | `{'PASS' if gate['passed'] else 'FAIL'}` |"
        )
    lines.append("")
    return "\n".join(lines)
