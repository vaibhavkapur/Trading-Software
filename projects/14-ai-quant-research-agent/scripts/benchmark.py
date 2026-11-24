"""Reproducible performance benchmark for the AI Quant Research Agent.

Reports machine details, workload shape, p50/p99 latencies (validation, full
4-year multi-symbol backtest + bootstrap, and report generation), error count,
and peak Python heap memory via tracemalloc.
"""

from __future__ import annotations

import platform
import sys
import tempfile
import time
import tracemalloc
from pathlib import Path

from quant_agent.api.app import ResearchApplicationState
from quant_agent.config import Settings


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    idx = min(len(ordered) - 1, int(round((pct / 100.0) * (len(ordered) - 1))))
    return ordered[idx]


def run_benchmark(iterations: int = 12) -> None:
    tracemalloc.start()
    val_ms: list[float] = []
    run_ms: list[float] = []
    report_ms: list[float] = []
    errors = 0

    with tempfile.TemporaryDirectory(prefix="quant_agent_bench_") as tmp:
        settings = Settings(data_dir=Path(tmp) / "data", code_revision="bench-0.1.0")
        state = ResearchApplicationState(settings=settings)
        try:
            # Warmup once
            h0, e0 = state.planner.propose("Does abnormal volume predict next-day returns?")
            state.registry.save_hypothesis(h0)
            state.registry.save_experiment(e0)
            state.validator.validate(e0)
            state.runner.submit(e0.id, async_mode=False)
            state.reporter.generate_report(h0.id)

            for i in range(iterations):
                try:
                    hyp, exp = state.planner.propose(
                        f"Does abnormal volume predict next-day returns (trial batch {i})?",
                        seed=20260100 + i,
                    )
                    state.registry.save_hypothesis(hyp)
                    state.registry.save_experiment(exp)

                    t0 = time.perf_counter()
                    v = state.validator.validate(exp)
                    val_ms.append((time.perf_counter() - t0) * 1000.0)
                    assert v.passed

                    t1 = time.perf_counter()
                    _, run = state.runner.submit(exp.id, async_mode=False)
                    run_ms.append((time.perf_counter() - t1) * 1000.0)
                    assert run is not None

                    t2 = time.perf_counter()
                    state.reporter.generate_report(hyp.id)
                    report_ms.append((time.perf_counter() - t2) * 1000.0)
                except Exception:
                    errors += 1
        finally:
            state.close()

    current_bytes, peak_bytes = tracemalloc.get_traced_memory()
    tracemalloc.stop()

    print("AI Quant Research Agent — Reproducible Benchmark Report")
    print("=" * 64)
    print(f"Python        : {sys.version.split()[0]}")
    print(f"Platform      : {platform.system()} {platform.release()} ({platform.machine()})")
    print(
        f"Workload      : {iterations} full pipelines (8 symbols, ~1,042 daily bars/symbol, "
        "train/val/test + 250-resample block bootstrap + SQLite/JSON artifacts)"
    )
    print(f"Errors        : {errors}")
    peak_mib = peak_bytes / (1024 * 1024)
    cur_mib = current_bytes / (1024 * 1024)
    print(f"Peak Heap Mem : {peak_mib:.2f} MiB (current {cur_mib:.2f} MiB)")
    print("-" * 64)
    print(
        f"Validate      : p50 = {percentile(val_ms, 50):7.2f} ms | "
        f"p99 = {percentile(val_ms, 99):7.2f} ms"
    )
    print(
        f"Backtest+Save : p50 = {percentile(run_ms, 50):7.2f} ms | "
        f"p99 = {percentile(run_ms, 99):7.2f} ms"
    )
    print(
        f"Report+Verify : p50 = {percentile(report_ms, 50):7.2f} ms | "
        f"p99 = {percentile(report_ms, 99):7.2f} ms"
    )
    print("=" * 64)


if __name__ == "__main__":
    run_benchmark()
